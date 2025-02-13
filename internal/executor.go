package internal

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/theckman/yacspin"
)

// This represent the default task, so when the user
// doesn't provide any args to the program, we default to this.
const DefaultTask = "main"

var spinnerCfg = yacspin.Config{
	Frequency:         100 * time.Millisecond,
	Colors:            []string{"fgYellow"},
	CharSet:           yacspin.CharSets[11],
	Suffix:            " ",
	SuffixAutoColon:   true,
	Message:           "Running commands",
	StopCharacter:     "✓",
	StopColors:        []string{"fgGreen"},
	StopMessage:       "Done",
	StopFailCharacter: "✗",
	StopFailColors:    []string{"fgRed"},
	StopFailMessage:   "Failed",
}

type Executor struct {
	parser   Parser
	lockfile Lockfile
	spinner  *yacspin.Spinner
	options  Options
}

// Executor constructor.
func NewExecutor(p *Parser, l *Lockfile, opts *Options) Executor {
	spinner, _ := yacspin.New(spinnerCfg)

	return Executor{
		parser:   *p,
		lockfile: *l,
		spinner:  spinner,
		options:  *opts,
	}
}

// Starts the command for a single run or as a watcher.
func (e *Executor) Start(taskName string) {
	arg := DefaultTask
	if taskName != "" {
		arg = taskName
	}

	if e.options.Watch {
		e.watch(arg)
	} else {
		if err := e.execute(arg); err != nil {
			e.logErr(err)
		}
	}
}

// Executes all command strings under given taskName.
// Each call happens in its own go routine.
func (e *Executor) execute(taskName string) error {
	task := e.initTask(taskName)
	didDispatch, err := e.checkAndDispatch(task)

	if err != nil {
		return err
	}

	if !didDispatch {
		e.logExit("success", "Nothing to run")
	}

	e.spinner.StopMessage("Done!")
	e.spinner.Stop()

	return nil
}

// Begins an infinite loop that watches for the file changes
// in the "files" section of the task's configuration.
func (e *Executor) watch(taskName string) {
	task := e.initTask(taskName)
	wait := make(chan struct{})

	for {
		go func(ch chan struct{}) {
			e.checkAndDispatch(task)
			e.spinner.Message("Watching for file changes...")

			time.Sleep(time.Second)
			ch <- struct{}{}
		}(wait)

		<-wait
	}
}

// Checks whether the task will be dispatched or not,
// and then dispatches is true. Returns true if dispatched.
func (e *Executor) checkAndDispatch(task Task) (bool, error) {
	shouldDispatch, err := e.shouldDispatch(task)
	if err != nil {
		return false, err
	}

	if shouldDispatch || e.options.Force {
		if err := e.dispatchTask(task, true); err != nil {
			return false, err
		}
	}

	return (shouldDispatch || e.options.Force), nil
}

// Fetch the task from the parser based on task name.
func (e *Executor) initTask(taskName string) Task {
	if !e.options.Quiet {
		e.spinner.Start()
	}

	e.mustExist(taskName)
	return e.parser.Tasks[taskName]
}

// Checks whether files have changed since the last run.
// Also updates the lockfile if files did get modified.
// If no "files" key is present in the task, simply returns true.
func (e *Executor) shouldDispatch(task Task) (bool, error) {
	if len(task.Files) == 0 {
		return true, nil
	}

	dispatchCh := make(chan Ref[bool])
	go e.shouldDispatchRoutine(task, dispatchCh)
	dispatch := <-dispatchCh

	if dispatch.Error() != nil {
		return false, dispatch.Error()
	}

	if dispatch.Value() {
		e.lockfile.UpdateTimestampsForFiles(task.Files)
	}

	return dispatch.Value(), nil
}

// Go Routine function that determines whether the stored
// mtime is greater  than mtime if the file at this moment.
func (e *Executor) shouldDispatchRoutine(task Task, ch chan Ref[bool]) {
	lockedModTimes := e.lockfile.GetCurrentProject()

	for _, f := range task.Files {
		fo, err := os.Stat(f)
		if err != nil {
			ch <- NewRef(false, err)
		}

		modTimeNow := fo.ModTime().Unix()
		if lockedModTimes[f] < modTimeNow {
			ch <- NewRef(true, nil)
			return
		}
	}

	ch <- NewRef(false, nil)
}

// Dispatches the individual commands of the current task,
// including any events that need to be run.
func (e *Executor) dispatchTask(task Task, initialRun bool) error {
	outputs := make(chan Ref[string])

	if initialRun {
		for _, beforeEachCmd := range e.parser.Global.Shared.Events.BeforeEachTask {
			err := e.runSysOrRecurse(beforeEachCmd, &outputs)

			if err != nil {
				return err
			}
		}
	}

	for _, mainCmd := range task.Run {
		if initialRun {
			for _, beforeEachCmd := range e.parser.Global.Shared.Events.BeforeEachRun {
				if err := e.runSysOrRecurse(beforeEachCmd, &outputs); err != nil {
					return err
				}
			}
		}

		if err := e.runSysOrRecurse(mainCmd, &outputs); err != nil {
			return err
		}

		if initialRun {
			for _, afterEachCmd := range e.parser.Global.Shared.Events.AfterEachRun {
				if err := e.runSysOrRecurse(afterEachCmd, &outputs); err != nil {
					return err
				}
			}
		}
	}

	for _, afterEachCmd := range e.parser.Global.Shared.Events.AfterEachTask {
		if err := e.runSysOrRecurse(afterEachCmd, &outputs); err != nil {
			return err
		}
	}

	return nil
}

// Determine what to execute: system command or another declared task in goke.yml.
func (e *Executor) runSysOrRecurse(cmd string, ch *chan Ref[string]) error {
	if !e.options.Quiet {
		e.spinner.Message(fmt.Sprintf("Running: %s", cmd))
	}

	if _, ok := e.parser.Tasks[cmd]; ok {
		return e.dispatchTask(e.parser.Tasks[cmd], false)
	} else {
		go e.runSysCommand(cmd, *ch)
		output := <-*ch

		if output.Error() != nil {
			return output.Error()
		}

		if !e.options.Quiet {
			fmt.Print(output.Value())
		}
	}

	return nil
}

// Executes the given string in the underlying OS.
func (e *Executor) runSysCommand(c string, ch chan Ref[string]) {
	splitCmd, err := ParseCommandLine(os.ExpandEnv(c))

	if err != nil {
		ch <- NewRef("", err)
		return
	}

	out, err := exec.Command(splitCmd[0], splitCmd[1:]...).Output()
	if err != nil {
		ch <- NewRef("", err)
		return
	}

	ch <- NewRef("\n"+string(out)+"\n", nil)
}

func (e *Executor) mustExist(taskName string) {
	if _, ok := e.parser.Tasks[taskName]; !ok {
		e.logExit("error", fmt.Sprintf("Command '%s' not found\n", taskName))
	}
}

// Shortcut to logging an error using spinner logger.
func (e *Executor) logErr(err error) {
	e.logExit("error", fmt.Sprintf("Error: %s\n", err.Error()))
}

// Log to the console using the spinner instance.
func (e *Executor) logExit(status string, message string) {
	switch status {
	default:
	case "success":
		if !e.options.Quiet {
			e.spinner.StopMessage(message)
			e.spinner.Stop()
		}
		os.Exit(0)
	case "error":
		if !e.options.Quiet {
			e.spinner.StopFailMessage(message)
			e.spinner.StopFail()
		}
		os.Exit(1)
	}
}
