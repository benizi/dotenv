package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/mattn/go-shellwords"
)

var (
	identifier = `[A-Za-z_][A-Za-z_0-9]*`
	name       = regexp.MustCompile(identifier)
	assignment = regexp.MustCompile(`^` + identifier + `=`)
	getID      = regexp.MustCompile(`^(` + identifier + `)=`)
	comment    = regexp.MustCompile(`^\s*#`)
	nonstrict  = regexp.MustCompile(`^[^\s=]+=`)
	usage      = `Usage: dotenv [options] [mode] [envs] [--] [cmd [args]]

Modes:
  -o (output) / -dump = dump all
  -n (names) / -names = print names of assigned vars
  -p (values) / -vals = print values of specified vars

Options:
  -s / -shell = Parse files as shell scripts ('export BLAH="value"')
  -a (alphanumeric) / -strict = Only accept simple names (` + identifier + `)
  -no-sort / -unsorted = Don't sort (default: do)
  -sort / -sorted = Sort output by default

Envs:
  NAME=VALUE
  filename
`
	alphanumeric = false
)

type debugging bool

var debug debugging

func (d debugging) Printf(format string, args ...interface{}) {
	if d {
		log.Printf(format, args...)
	}
}

type sourcetype string

const (
	notype sourcetype = "notype"
	file              = "file"
	shell             = "shell"
	raw               = "raw"
	osenv             = "osenv"
)

func (kind sourcetype) rank() int {
	switch kind {
	case raw:
		return 0
	case osenv:
		return 1
	case file, shell:
		return 2
	default:
		return 3
	}
}

func (kind sourcetype) reverse() bool {
	return kind == raw
}

type varsource struct {
	data     string
	kind     sourcetype
	explicit bool
	optional bool
}

func (src varsource) parse() ([]string, error) {
	switch src.kind {
	case file:
		return src.parseFile()
	case shell:
		return src.parseShell()
	case raw:
		return []string{src.data}, nil
	case osenv:
		return os.Environ(), nil
	}
	return nil, fmt.Errorf("Unknown varsource kind: %v (data: %v)", src.kind, src.data)
}

func (src varsource) parseFile() ([]string, error) {
	var vars []string
	file, err := os.Open(src.data)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)
	matcher := nonstrict
	if alphanumeric {
		matcher = assignment
	}
	for scanner.Scan() {
		line := scanner.Text()
		if comment.MatchString(line) {
			continue
		}
		if matcher.MatchString(line) {
			vars = append(vars, line)
		}
	}
	return vars, nil
}

func (src varsource) parseShell() ([]string, error) {
	debug.Printf("Trying Shell: %s\n", src.data)
	var vars []string
	file, err := os.Open(src.data)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	parser := shellwords.NewParser()
	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		line := scanner.Text()
		if comment.MatchString(line) {
			continue
		}
		tokens, err := parser.Parse(line)
		for err != nil && scanner.Scan() {
			line = line + "\n" + scanner.Text()
			tokens, err = parser.Parse(line)
		}
		if err != nil {
			debug.Printf("Skipping [%s]\n", line)
			continue
		}
		if len(tokens) > 0 && tokens[0] == "export" {
			tokens = tokens[1:]
		}
		if len(tokens) == 0 {
			continue
		}
		if assignment.MatchString(tokens[0]) {
			vars = append(vars, tokens[0])
		} else if name.MatchString(tokens[0]) && len(tokens) > 1 {
			key := tokens[0]
			val := tokens[1]
			if val == "=" && len(tokens) > 2 {
				val = tokens[2]
			} else {
				debug.Printf("TODO: %q\n", tokens)
			}
			vars = append(vars, fmt.Sprintf("%s=%s", key, val))
		} else {
			debug.Printf("TODO: %q\n", tokens)
			continue
		}
	}
	return vars, nil
}

type priority struct {
	source varsource
	pos    int
}

type prioritysort struct {
	sources []priority
}

func (p *prioritysort) Len() int {
	return len(p.sources)
}
func (p *prioritysort) Swap(i, j int) {
	p.sources[i], p.sources[j] = p.sources[j], p.sources[i]
}
func (p *prioritysort) Less(i, j int) bool {
	a, b := p.sources[i], p.sources[j]
	ka, kb := a.source.kind, b.source.kind
	ra, rb := ka.rank(), kb.rank()
	pa, pb := a.pos, b.pos
	switch {
	case ra != rb:
		return ra < rb
	case ka.reverse():
		return pb < pa
	default:
		return pa < pb
	}
}

func (p *prioritysort) sort() []varsource {
	sort.Sort(p)
	ret := []varsource{}
	for _, i := range p.sources {
		ret = append(ret, i.source)
	}
	return ret
}

func bypriority(sources []varsource) *prioritysort {
	p := &prioritysort{}
	for i, s := range sources {
		p.sources = append(p.sources, priority{s, i})
	}
	return p
}

func uniqVarsByName(allvars []string) ([]string, []string) {
	vars := []string{}
	varnames := []string{}
	varindex := map[string]int{}

	for _, v := range allvars {
		name := v
		match := getID.FindStringSubmatch(v)
		if match != nil {
			name = match[1]
		}
		_, seen := varindex[name]
		if !seen {
			varnames = append(varnames, name)
			varindex[name] = len(vars)
			vars = append(vars, v)
		}
	}

	return varnames, vars
}

type operation string

const (
	runcmd operation = "runcmd"
	dump             = "dump"
	names            = "names"
	values           = "values"
)

func main() {
	debug = os.Getenv("DEBUG") != ""
	args := os.Args[1:]
	mode := runcmd
	var defaultType sourcetype
	defaultType = file
	specifiedDefault := false
	sorted := true
	clearEnv := false
	var cmd []string
	var sources []varsource
	var vars []string

	doSplit, splitIndex := false, 0
	for i, arg := range args {
		if arg == "--" {
			doSplit, splitIndex = true, i
			break
		}
	}
	if doSplit {
		args, cmd = args[0:splitIndex], args[splitIndex+1:]
	}

	debug.Printf("Pre-source parsing:")
	debug.Printf("args: %q\n", args)
	debug.Printf("cmd: %q\n", cmd)

	setDefaultType := func(t sourcetype) {
		defaultType = t
		for i, s := range sources {
			if s.kind == notype {
				sources[i].kind = defaultType
			}
		}
		specifiedDefault = true
	}

	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		debug.Printf("arg[%s] args[%v]", arg, args)
		source := varsource{kind: notype, data: arg}
		if specifiedDefault {
			source.kind = defaultType
		}
		if strings.HasPrefix(arg, "--") {
			arg = arg[1:]
		}
		if arg == "-h" || arg == "-help" {
			os.Stdout.Write([]byte(usage))
			os.Exit(0)
		} else if arg == "-f" {
			debug.Printf("[%s] = File flag", arg)
			if len(args) == 0 {
				log.Fatal("Flag `-f` requires a filename")
			}
			source.data = args[0]
			args = args[1:]
			source.explicit = true
		} else if arg == "-o" || arg == "-dump" {
			mode = dump
			continue
		} else if arg == "-n" || arg == "-names" {
			mode = names
			continue
		} else if arg == "-p" || arg == "-vals" {
			mode = values
			continue
		} else if arg == "-s" || arg == "-shell" {
			setDefaultType(shell)
			continue
		} else if arg == "-a" || arg == "-strict" {
			alphanumeric = true
			continue
		} else if arg == "-no-sort" || arg == "-unsorted" {
			sorted = true
			continue
		} else if arg == "-sort" || arg == "-sorted" {
			sorted = false
			continue
		} else if arg == "-u" || arg == "-clear" {
			clearEnv = true
			continue
		} else if assignment.MatchString(arg) {
			debug.Printf("[%s] = raw assignment", arg)
			source.kind = raw
		} else if doSplit {
			debug.Printf("[%s] = pre-split file source", arg)
			source.explicit = true
		} else {
			debug.Printf("[%s] = attempt file", arg)
		}
		debug.Printf("adding source: %#+v\n", source)
		sources = append(sources, source)
	}

	setDefaultType(defaultType)

	debug.Printf("Prepending osenv?: %v\n", !clearEnv)
	if !clearEnv {
		sources = append([]varsource{{kind: osenv}}, sources...)
	}

	debug.Printf("Sources: %#+v\n", sources)

	debug.Printf("bypriority: %#+v\n", bypriority(sources))
	for i, s := range bypriority(sources).sources {
		debug.Printf(" [%d]=%#+v\n", i, s)
	}
	sources = bypriority(sources).sort()

	debug.Printf("Sorted: %#+v\n", sources)

	for i, source := range sources {
		parsed, err := source.parse()
		if err != nil && source.explicit {
			log.Printf("Failed to read source: %#+v", source)
			log.Fatalf("Error was: %v", err)
		} else if err != nil && !source.optional {
			debug.Printf("Failed to read source: %#+v", source)
			debug.Printf("Treating as cmd.")
			precmd := []string{}
			for _, source := range sources[i:] {
				precmd = append(precmd, source.data)
			}
			cmd = append(precmd, cmd...)
			break
		} else if err != nil {
			debug.Printf("Failed to read source: %#+v", source)
			debug.Printf("Ignoring.")
			fmt.Fprintf(os.Stderr, "Error parsing %s: %v\n", source.data, err)
		}
		vars = append(vars, parsed...)
	}

	var varnames []string
	varnames, vars = uniqVarsByName(vars)

	if len(cmd) == 0 {
		cmd = []string{"sh"}
	}

	debug.Printf("Post-source parsing:")
	debug.Printf("args: %q\n", args)
	debug.Printf("cmd: %q\n", cmd)
	debug.Printf("mode: %s\n", mode)

	var toDump []string
	dumping := true
	switch mode {
	case dump:
		toDump = vars
	case names:
		toDump = varnames
	case values:
		for _, key := range cmd {
			found := false
			prefix := key + "="
			for _, val := range vars {
				if strings.HasPrefix(val, prefix) {
					toDump = append(toDump, val[len(prefix):])
					found = true
				}
			}
			if !found {
				val := os.Getenv(key)
				if val != "" {
					toDump = append(toDump, val)
					found = true
				}
			}
			if !found {
				log.Printf("Variable not set by dotenv: %s", key)
			}
		}
	case runcmd:
		dumping = false
	}

	if dumping {
		if sorted {
			sort.Strings(toDump)
		}
		for _, line := range toDump {
			fmt.Println(line)
		}
		return
	}

	proc := exec.Command(cmd[0], cmd[1:]...)
	proc.Stdin = os.Stdin
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	proc.Env = vars
	if err := proc.Start(); err != nil {
		log.Fatalf("proc.Start: %v", err)
	}
	if err := proc.Wait(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok {
				os.Exit(status.ExitStatus())
			}
		}
	}
}
