package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
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
  -s / --shell = Parse files as shell scripts ('export BLAH="value"')
  -a (alphanumeric) / --strict-vars = Only accept simple names (` + identifier + `)
  --no-sort / --unsorted = Don't sort (default: do)
  --sort / --sorted = Sort output by default

Envs:
  NAME=VALUE
  filename
`
	alphanumeric = false
)

// Variables for parsing Python-dotenv-style files "lax" = poorly-defined
var (
	laxID      = regexp.MustCompile(`^(?:[^\S\n]*export\b)?[^\S\n]*([^\s=#]+)`)
	laxequals  = regexp.MustCompile(`^[^\S\n]*=[^\S\n]*`)
	laxempty   = regexp.MustCompile(`^[^\S\n]*(\n|$)`)
	laxcomment = regexp.MustCompile(`^[^\S\n]*#[^\n]*(\n|$)`)
	laxtrailer = regexp.MustCompile(`^((?s:.)+?)\s+#`)
	laxqstart  = regexp.MustCompile(`^(['"])`)
	laxescaped = regexp.MustCompile(`\\(?s:.)`)
	laxsingleq = regexp.MustCompile(`^((?:[^\\']|\\(?s:.))*)'`)
	laxdoubleq = regexp.MustCompile(`^((?:[^\\"]|\\(?s:.))*)"`)
	laxdiscard = regexp.MustCompile(`^([^\n]*)(?:\n|$)`)
	tointerp   = regexp.MustCompile(`\$\{([^}]+)\}`)
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
	notype  sourcetype = "notype"
	file               = "file"
	shell              = "shell"
	raw                = "raw"
	osenv              = "osenv"
	laxfile            = "laxfile"
)

func (kind sourcetype) rank() int {
	switch kind {
	case raw:
		return 0
	case osenv:
		return 1
	case file, shell, laxfile:
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

type envvar struct {
	name, val string
	allowsubs bool
}

func parsevar(s string) envvar {
	parts := strings.SplitN(s, "=", 2)
	name, val := "", ""
	if len(parts) > 0 {
		name = parts[0]
	}
	if len(parts) > 1 {
		val = parts[1]
	}
	return envvar{name, val, false}
}

func (src varsource) parse() ([]envvar, error) {
	switch src.kind {
	case file:
		return src.parseFile()
	case shell:
		return src.parseShell()
	case laxfile:
		return src.parseLax()
	case raw:
		return []envvar{parsevar(src.data)}, nil
	case osenv:
		return src.parseOsEnviron()
	}
	return nil, fmt.Errorf("Unknown varsource kind: %v (data: %v)", src.kind, src.data)
}

func (src varsource) parseFile() ([]envvar, error) {
	var vars []envvar
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
			vars = append(vars, parsevar(line))
		}
	}
	return vars, nil
}

func (src varsource) parseShell() ([]envvar, error) {
	debug.Printf("Trying Shell: %s\n", src.data)
	var vars []envvar
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
			vars = append(vars, parsevar(tokens[0]))
		} else if name.MatchString(tokens[0]) && len(tokens) > 1 {
			key := tokens[0]
			val := tokens[1]
			if val == "=" && len(tokens) > 2 {
				val = tokens[2]
			} else {
				debug.Printf("TODO: %q\n", tokens)
			}
			vars = append(vars, envvar{key, val, true})
		} else {
			debug.Printf("TODO: %q\n", tokens)
			continue
		}
	}
	return vars, nil
}

func dbglines(s string) string {
	lines := strings.SplitN(s, "\n", 4)
	if len(lines) > 3 {
		lines = lines[0:2]
	}
	return strings.Join(lines, "\n")
}

// Find regex submatches, but also trim them off the front of the string
func trimRegexMatches(s *string, r *regexp.Regexp) (bool, []string) {
	matches := r.FindStringSubmatch(*s)
	if matches == nil {
		return false, nil
	}
	*s = (*s)[len(matches[0]):]
	return true, matches
}

// Trim a match off the front of the string, but just return whether it matched
func trimRegex(s *string, r *regexp.Regexp) bool {
	matched, _ := trimRegexMatches(s, r)
	return matched
}

var (
	// substitutions valid for single-quoted strings
	laxsqsubs = map[byte]string{
		'\'': "'",
		'\\': "\\",
	}
	// substitutions valid for double-quoted strings
	laxdqsubs = map[byte]string{
		'\'': "'",
		'\\': "\\",
		'"':  "\"",
		'a':  "\a",
		'b':  "\b",
		'f':  "\f",
		'n':  "\n",
		'r':  "\r",
		't':  "\t",
		'v':  "\v",
	}
)

func laxsubsq(e string) string {
	r, ok := laxsqsubs[e[1]]
	if ok {
		return r
	}
	return e
}

func laxparsesq(s string) string {
	return laxescaped.ReplaceAllStringFunc(s, laxsubsq)
}

func laxsubdq(e string) string {
	r, ok := laxdqsubs[e[1]]
	if ok {
		return r
	}
	return e
}

func laxparsedq(s string) string {
	return laxescaped.ReplaceAllStringFunc(s, laxsubdq)
}

// Parse a Python-dotenv style file (allows some quoting, interpolation)
func (src varsource) parseLax() ([]envvar, error) {
	vars := []envvar{}
	rawdata, err := ioutil.ReadFile(src.data)
	if err != nil {
		return nil, err
	}
	data := string(rawdata)
	for len(data) > 0 {
		debug.Printf("")
		debug.Printf("PARSING %q", dbglines(data))
		lines := strings.SplitN(data, "\n", 2)
		line := lines[0]
		if trimRegex(&data, laxcomment) {
			debug.Printf("  COMMENT[%s]", line)
			continue
		}
		if trimRegex(&data, laxempty) {
			debug.Printf("  EMPTYLINE[%q]", line)
			continue
		}
		hasID, idmatch := trimRegexMatches(&data, laxID)
		debug.Printf("  ID?(%v) [%#+v]", hasID, idmatch)
		name, val, allowsubs := "", "", true
		switch {
		case hasID:
			name = idmatch[1]
			debug.Printf("  HASID NAME[%s]", name)
			switch {
			case trimRegex(&data, laxcomment):
				debug.Printf("EMPTYCOMM[%s]", line)
			case trimRegex(&data, laxempty):
				debug.Printf("EMPTYVAL[%s]", line)
			case trimRegex(&data, laxequals):
				debug.Printf("  HASEQ remaining:[%q]", dbglines(data))
				hasQ, qmatch := trimRegexMatches(&data, laxqstart)
				if hasQ {
					qkind, qmatcher, unquoter := "double", laxdoubleq, laxparsedq
					if qmatch[1] == "'" {
						qkind, qmatcher, unquoter = "single", laxsingleq, laxparsesq
						allowsubs = false
					}
					hasMatch, qvals := trimRegexMatches(&data, qmatcher)
					if !hasMatch {
						debug.Printf("Unclosed %s-quoted value [%q]", qkind, data)
						return nil, fmt.Errorf("Unclosed %s-quoted value", qkind)
					}
					val = unquoter(qvals[1])
					debug.Printf("%s-QUOTED RAW[%q] VAL[%q]", strings.ToUpper(qkind), qvals[1], val)
					debug.Printf("  BEFORE[%q]", dbglines(data))
					if !trimRegex(&data, laxcomment) {
						trimRegex(&data, laxdiscard)
					}
					debug.Printf("  AFTER [%q]", dbglines(data))
				} else {
					toend, lvals := trimRegexMatches(&data, laxdiscard)
					if !toend {
						return nil, fmt.Errorf("Couldn't read to end [%q]", data)
					}
					val = strings.TrimSpace(lvals[1])
					trailmatch := laxtrailer.FindStringSubmatch(val)
					if trailmatch != nil {
						val = trailmatch[1]
					}
					debug.Printf("SIMPLEVAL[%q]", val)
				}
			default:
				log.Printf("Invalid line (%q)", line)
				debug.Printf("FIXME")
				trimRegex(&data, laxdiscard)
				continue
			}
		default:
			log.Printf("Invalid line (%q)", line)
			trimRegex(&data, laxdiscard)
			continue
		}
		vars = append(vars, envvar{name, val, allowsubs})
	}
	return vars, nil
}

func (src varsource) parseOsEnviron() ([]envvar, error) {
	vars := []envvar{}
	for _, s := range os.Environ() {
		vars = append(vars, parsevar(s))
	}
	return vars, nil
}

func substitutevars(env, raw []envvar) []envvar {
	parsed := []envvar{}
	vals := map[string]string{}
	// Include original env vars, even if they're being cleared
	for _, v := range os.Environ() {
		e := parsevar(v)
		vals[e.name] = e.val
	}
	for _, v := range env {
		vals[v.name] = v.val
	}
	for _, r := range raw {
		subbed := r.val
		if r.allowsubs {
			subbed = tointerp.ReplaceAllStringFunc(subbed, func(s string) string {
				parts := tointerp.FindStringSubmatch(s)
				if len(parts) > 1 {
					if val, ok := vals[parts[1]]; ok {
						return val
					}
				}
				return ""
			})
		}
		vals[r.name] = subbed
		parsed = append(parsed, envvar{r.name, subbed, r.allowsubs})
	}
	return parsed
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

func uniqVarsByName(allvars []envvar) ([]string, []envvar) {
	vars := []envvar{}
	varnames := []string{}
	varindex := map[string]int{}

	for _, v := range allvars {
		_, seen := varindex[v.name]
		if !seen {
			varnames = append(varnames, v.name)
			varindex[v.name] = len(vars)
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
	defaultType = laxfile
	specifiedDefault := false
	sorted := true
	clearEnv := false
	var cmd []string
	var sources []varsource
	var vars []envvar

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
		} else if arg == "-x" || arg == "-strict" {
			setDefaultType(file)
			continue
		} else if arg == "-a" || arg == "-strict-vars" {
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
		parsed = substitutevars(vars, parsed)
		vars = append(vars, parsed...)
	}

	_, vars = uniqVarsByName(vars)

	if len(cmd) == 0 {
		cmd = []string{"sh"}
	}

	debug.Printf("Post-source parsing:")
	debug.Printf("args: %q\n", args)
	debug.Printf("cmd: %q\n", cmd)
	debug.Printf("mode: %s\n", mode)

	var toDump []envvar
	dumping := true
	switch mode {
	case dump, names:
		toDump = vars
	case values:
		for _, key := range cmd {
			found := false
			for _, v := range vars {
				if v.name == key {
					toDump = append(toDump, v)
					found = true
				}
			}
			if !found {
				val := os.Getenv(key)
				if val != "" {
					toDump = append(toDump, parsevar(val))
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
			dumpnames := []string{}
			byname := map[string]envvar{}
			sorteddump := []envvar{}
			for _, v := range toDump {
				dumpnames = append(dumpnames, v.name)
				byname[v.name] = v
			}
			sort.Strings(dumpnames)
			for _, n := range dumpnames {
				sorteddump = append(sorteddump, byname[n])
			}
			toDump = sorteddump
		}
		for _, v := range toDump {
			switch mode {
			case dump:
				fmt.Printf("%s=%s\n", v.name, v.val)
			case names:
				fmt.Println(v.name)
			case values:
				fmt.Println(v.val)
			}
		}
		return
	}

	proc := exec.Command(cmd[0], cmd[1:]...)
	proc.Stdin = os.Stdin
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	env := []string{}
	for _, v := range vars {
		env = append(env, fmt.Sprintf("%s=%s", v.name, v.val))
	}
	proc.Env = env
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
