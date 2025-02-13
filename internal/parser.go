package internal

import (
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type (
	Task struct {
		Name  string
		Files []string          `yaml:"files,omitempty"`
		Run   []string          `yaml:"run"`
		Env   map[string]string `yaml:"env,omitempty"`
	}

	Global struct {
		Shared struct {
			Environment map[string]string `yaml:"environment,omitempty"`
			Events      struct {
				BeforeEachRun  []string `yaml:"before_each_run,omitempty"`
				AfterEachRun   []string `yaml:"after_each_run,omitempty"`
				BeforeEachTask []string `yaml:"before_each_task,omitempty"`
				AfterEachTask  []string `yaml:"after_each_task,omitempty"`
			} `yaml:"events,omitempty"`
		} `yaml:"global,omitempty"`
	}

	Parser struct {
		Tasks     taskList
		FilePaths []string
		config    string
		options   Options
		fs        FileSystem
		Global
	}

	taskList map[string]Task
)

var osCommandRegexp = regexp.MustCompile(`\$\((.+)\)`)
var parserString string

// NewParser creates a parser instance which can be either a blank one,
// or one provided  from the cache, which gets deserialized.
func NewParser(cfg string, opts *Options, fs FileSystem) Parser {
	p := Parser{}
	p.fs = fs
	p.config = cfg
	p.options = *opts

	tempFile := path.Join(p.fs.TempDir(), p.getTempFileName())

	if p.shouldClearCache(tempFile) {
		_ = p.fs.Remove(tempFile)
	}

	if !p.fs.FileExists(tempFile) {
		return p
	}

	pBytes, err := p.fs.ReadFile(tempFile)
	if err != nil && !opts.Quiet {
		log.Fatal(err)
	}

	pStr := string(pBytes)
	parserString = pStr

	return GOBDeserialize(pStr, &p)
}

// Bootstrap does the parsing process or skip if cached.
func (p *Parser) Bootstrap() {
	// Nothing too bootstrap if cached.
	if parserString != "" {
		return
	}

	err := p.parseGlobal()
	if err != nil && !p.options.Quiet {
		log.Fatal(err)
	}

	err = p.parseTasks()
	if err != nil && !p.options.Quiet {
		log.Fatal(err)
	}

	pStr := GOBSerialize(*p)
	err = p.fs.WriteFile(path.Join(p.fs.TempDir(), p.getTempFileName()), []byte(pStr), 0644)

	if err != nil && !p.options.Quiet {
		log.Fatal(err)
	}
}

// Parses the individual user defined tasks in the YAML config,
// and processes the dynamic parts of both "run" and "files" sections.
func (p *Parser) parseTasks() error {
	var tasks taskList

	if err := yaml.Unmarshal([]byte(p.config), &tasks); err != nil {
		return err
	}

	allFilesPaths := []string{}

	for k, c := range tasks {
		filePaths := []string{}
		for i := range c.Files {
			p.replaceEnvironmentVariables(osCommandRegexp, &tasks[k].Files[i])
			expanded, err := p.expandFilePaths(tasks[k].Files[i])

			if err != nil {
				return err
			}

			filePaths = append(filePaths, expanded...)
			allFilesPaths = append(allFilesPaths, expanded...)
		}

		c.Files = filePaths
		tasks[k] = c

		for i, r := range c.Run {
			tasks[k].Run[i] = strings.Replace(r, "{FILES}", strings.Join(c.Files, " "), -1)
			p.replaceEnvironmentVariables(osCommandRegexp, &tasks[k].Run[i])
		}

		if len(c.Env) != 0 {
			vars, err := p.setEnvVariables(c.Env)
			if err != nil {
				return err
			}
			c.Env = vars
		}
		c.Name = k
		tasks[k] = c
	}

	p.FilePaths = allFilesPaths
	p.Tasks = tasks

	return nil
}

// Parses the "global" key in the yaml config and adds it to the parser.
// Also sets all variables under global.environment as OS environment variables.
func (p *Parser) parseGlobal() error {
	var g Global

	if err := yaml.Unmarshal([]byte(p.config), &g); err != nil {
		return err
	}

	vars, err := p.setEnvVariables(g.Shared.Environment)
	if err != nil {
		return nil
	}

	g.Shared.Environment = vars
	p.Global = g

	return nil
}

// Parses the interpolated system commands, ie. "Hello $(echo 'World')" and returns it.
// Returns the command wrapper in $() and without the wrapper.
func (p *Parser) parseSystemCmd(re *regexp.Regexp, str string) (string, string) {
	match := re.FindAllStringSubmatch(str, -1)

	if len(match) > 0 && len(match[0]) > 0 {
		return match[0][0], match[0][1]
	}

	return "", ""
}

// Replace the placeholders with actual environment variable values in string pointer.
// Given that a string pointer must be provided, the replacement happens in place.
func (p *Parser) replaceEnvironmentVariables(re *regexp.Regexp, str *string) {
	resolved := *str
	raw, env := p.parseSystemCmd(re, resolved)

	if raw != "" && env != "" {
		*str = strings.Replace(resolved, raw, os.Getenv(env), -1)
	}
}

// Expand the path glob and returns all paths in an array
func (p *Parser) expandFilePaths(file string) ([]string, error) {
	filePaths := []string{}

	if strings.Contains(file, "*") {
		files, err := p.fs.Glob(file)
		if err != nil {
			return nil, err
		}

		if len(files) > 0 {
			filePaths = append(filePaths, files...)
		}
	} else if p.fs.FileExists(file) {
		filePaths = append(filePaths, file)
	}

	return filePaths, nil
}

// Retrieves the temp file name
func (p *Parser) getTempFileName() string {
	cwd, _ := p.fs.Getwd()
	return "goke-" + strings.Replace(cwd, string(filepath.Separator), "-", -1)
}

// Determines whether the parser cache should be cleaned or not
func (p *Parser) shouldClearCache(tempFile string) bool {
	tempFileExists := p.fs.FileExists(tempFile)
	mustCleanCache := false

	if !p.options.ClearCache && tempFileExists {
		tempStat, _ := p.fs.Stat(tempFile)
		tempModTime := tempStat.ModTime().Unix()

		configStat, _ := p.fs.Stat(CurrentConfigFile())
		configModTime := configStat.ModTime().Unix()

		mustCleanCache = tempModTime < configModTime
	}

	if p.options.ClearCache && tempFileExists {
		mustCleanCache = true
	}

	return mustCleanCache
}

// prase system commands and store results to env
func (p *Parser) setEnvVariables(vars map[string]string) (map[string]string, error) {
	retVars := make(map[string]string)
	for k, v := range vars {
		_, cmd := p.parseSystemCmd(osCommandRegexp, v)

		if cmd == "" {
			retVars[k] = v
			_ = os.Setenv(k, v)
			continue
		}

		splitCmd, err := ParseCommandLine(os.ExpandEnv(cmd))
		if err != nil {
			return retVars, err
		}

		out, err := exec.Command(splitCmd[0], splitCmd[1:]...).Output()
		if err != nil {
			return retVars, err
		}

		outStr := strings.TrimSpace(string(out))
		retVars[k] = outStr
		_ = os.Setenv(k, outStr)
	}

	return retVars, nil
}
