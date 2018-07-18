package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"syscall"

	"github.com/mattn/go-shellwords"
)

var (
	identifier = `[A-Za-z_][A-Za-z_0-9]*`
	name       = regexp.MustCompile(identifier)
	assignment = regexp.MustCompile(`^` + identifier + `=`)
	getID      = regexp.MustCompile(`^(` + identifier + `)=`)
	comment    = regexp.MustCompile(`^\s*#`)
)

type debugging bool

var debug debugging

func (d debugging) Printf(format string, args ...interface{}) {
	if d {
		log.Printf(format, args...)
	}
}

type sourcetype int

const (
	file sourcetype = iota
	raw
)

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
	case raw:
		return []string{src.data}, nil
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
	parser := shellwords.NewParser()
	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		line := scanner.Text()
		if comment.MatchString(line) {
			continue
		}
		tokens, err := parser.Parse(line)
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

func main() {
	debug = os.Getenv("DEBUG") != ""
	args := os.Args[1:]
	var cmd []string
	var sources []varsource
	var vars []string
	justOutput := false

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

	addDefault := true
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		debug.Printf("arg[%s] args[%v]", arg, args)
		source := varsource{kind: file, data: arg}
		if arg == "-f" {
			debug.Printf("[%s] = File flag", arg)
			if len(args) == 0 {
				log.Fatal("Flag `-f` requires a filename")
			}
			source.data = args[0]
			args = args[1:]
			source.explicit = true
		} else if arg == "-o" {
			justOutput = true
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
		sources = append(sources, source)
		if source.explicit {
			addDefault = false
		}
	}

	if addDefault {
		dotenv := varsource{kind: file, data: ".env", optional: true}
		sources = append([]varsource{dotenv}, sources...)
	}

	debug.Printf("Sources: %#+v\n", sources)

	for len(sources) > 0 {
		source := sources[0]
		parsed, err := source.parse()
		if err != nil && source.explicit {
			log.Printf("Failed to read source: %#+v", source)
			log.Fatalf("Error was: %v", err)
		} else if err != nil && !source.optional {
			debug.Printf("Failed to read source: %#+v", source)
			debug.Printf("Treating as cmd.")
			precmd := []string{}
			for _, source := range sources {
				precmd = append(precmd, source.data)
				sources = []varsource{}
			}
			cmd = append(precmd, cmd...)
			break
		} else if err != nil {
			debug.Printf("Failed to read source: %#+v", source)
			debug.Printf("Ignoring.")
			fmt.Fprintf(os.Stderr, "Error parsing %s: %v\n", source.data, err)
		}
		vars = append(vars, parsed...)
		sources = sources[1:]
	}

	if len(cmd) == 0 {
		cmd = []string{"env"}
	}

	debug.Printf("Post-source parsing:")
	debug.Printf("args: %q\n", args)
	debug.Printf("cmd: %q\n", cmd)

	if justOutput {
		for _, env := range vars {
			fmt.Println(env)
		}
		return
	}

	proc := exec.Command(cmd[0], cmd[1:]...)
	proc.Stdin = os.Stdin
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	proc.Env = append(os.Environ(), vars...)
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
