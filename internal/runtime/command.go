package runtime

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime" // aliased: this package is also called runtime
	"strings"
	"sync"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// CommandRuntime shells out to a real coding agent CLI.
//
// The command is a template rather than hard-coded flags, because agent CLIs
// change their interfaces often. If your `codex` or `claude` build wants
// different arguments, override the template instead of editing this file.
type CommandRuntime struct {
	// RuntimeName is what gets recorded on the worker row ("codex", "claude",
	// "shell", ...).
	RuntimeName string
	// Template is the command line to run. Placeholders:
	//   {{prompt_file}}  absolute path to the prompt inside the worktree
	//   {{worktree}}     absolute path to the worktree
	//   {{task_id}}      the task ID
	Template string
	// Stdin feeds the prompt to the command on standard input instead of
	// relying on the command to read the file.
	Stdin bool
}

// DefaultTemplates are the starting points for the built-in agent runtimes.
// They mirror the documented invocations; adjust with --runtime-command if
// your installed CLI differs.
var DefaultTemplates = map[string]struct {
	Template string
	Stdin    bool
}{
	Codex:  {Template: `codex exec -- {{prompt_file}}`, Stdin: false},
	Claude: {Template: `claude --print`, Stdin: true},
}

// NewCommandRuntime builds a runtime for name, applying the default template
// when override is empty.
func NewCommandRuntime(name, override string, stdin bool) (*CommandRuntime, error) {
	tmpl := override
	useStdin := stdin

	if tmpl == "" {
		def, ok := DefaultTemplates[name]
		if !ok {
			return nil, fmt.Errorf("runtime %q has no default command; pass --runtime-command", name)
		}
		tmpl = def.Template
		if !stdin {
			useStdin = def.Stdin
		}
	}
	return &CommandRuntime{RuntimeName: name, Template: tmpl, Stdin: useStdin}, nil
}

// Name implements Runtime.
func (c *CommandRuntime) Name() string { return c.RuntimeName }

// Available checks the executable exists before any task is claimed, so the
// failure message arrives at startup instead of halfway through a task.
func (c *CommandRuntime) Available() error {
	fields := strings.Fields(c.Template)
	if len(fields) == 0 {
		return fmt.Errorf("runtime command template is empty")
	}
	bin := fields[0]
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("runtime %q needs %q on PATH but it was not found: %w\n"+
			"install it, override with --runtime-command, or run with --runtime fake to try the demo",
			c.RuntimeName, bin, err)
	}
	return nil
}

// Run implements Runtime.
func (c *CommandRuntime) Run(rc RunContext) (Result, error) {
	cmdline := c.Template
	for placeholder, value := range map[string]string{
		"{{prompt_file}}": rc.PromptFile,
		"{{worktree}}":    rc.WorktreeDir,
		"{{task_id}}":     rc.Task.ID,
	} {
		cmdline = strings.ReplaceAll(cmdline, placeholder, value)
	}

	shell, shellFlag := shellCommand()
	cmd := exec.CommandContext(rc.Ctx, shell, shellFlag, cmdline)
	cmd.Dir = rc.WorktreeDir
	// The agent runs inside the worktree and should stay there.
	cmd.Env = append(os.Environ(),
		"FACTORY_TASK_ID="+rc.Task.ID,
		"FACTORY_WORKTREE="+rc.WorktreeDir,
		"FACTORY_PROMPT_FILE="+rc.PromptFile,
	)
	if c.Stdin {
		cmd.Stdin = strings.NewReader(rc.Prompt)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}

	rc.Log("running %s runtime: %s", c.RuntimeName, cmdline)
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start runtime command: %w", err)
	}

	// Stream both streams into the task log as they arrive, so a long-running
	// agent is observable through `codefactory task show` while it works.
	var wg sync.WaitGroup
	wg.Add(2)
	go streamLines(&wg, stdout, "stdout", rc.Log)
	go streamLines(&wg, stderr, "stderr", rc.Log)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return Result{}, fmt.Errorf("runtime command failed: %w", err)
	}

	res := Result{Summary: fmt.Sprintf("%s runtime finished", c.RuntimeName)}

	// A refine task must leave the structured ticket behind. If the agent did
	// not produce one, that is a needs-human outcome rather than a crash.
	if rc.Task.Kind == api.KindRefineTicket {
		data, readErr := os.ReadFile(filepath.Join(rc.WorktreeDir, ".factory-refined.md"))
		if readErr != nil || strings.TrimSpace(string(data)) == "" {
			res.NeedsHuman = true
			res.Reason = "the agent did not write .factory-refined.md, so there is no ticket to publish"
			res.Summary = res.Reason
			return res, nil
		}
		res.RefinedTicket = string(data)
		if strings.Contains(strings.ToUpper(res.RefinedTicket), "BLOCKED") {
			res.NeedsHuman = true
			res.Reason = "the agent marked the ticket BLOCKED"
		}
	}
	return res, nil
}

func streamLines(wg *sync.WaitGroup, r interface{ Read([]byte) (int, error) }, label string, log func(string, ...any)) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		log("[%s] %s", label, sc.Text())
	}
}

func shellCommand() (string, string) {
	if goruntime.GOOS == "windows" {
		return "cmd", "/C"
	}
	return "sh", "-c"
}
