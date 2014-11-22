package main

import (
	"bytes"
	"go/build"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/juju/charm.v4"
	"gopkg.in/yaml.v1"
	"launchpad.net/errgo/errors"
)

const (
	hookPackage    = "github.com/juju/gocharm/hook"
	autogenMessage = `This file is automatically generated. Do not edit.`
	godepPath      = `github.com/tools/godep`
)

var hookMainCode = template.Must(template.New("").Parse(`
// {{.AutogenMessage}}

package main

import (
	"fmt"
	"os"
	charm {{.CharmPackage | printf "%q"}}
	{{.HookPackage | printf "%q"}}
)

func nop() error {
	return nil
}

func main() {
	r := hook.NewRegistry()
	charm.RegisterHooks(r)
	hook.RegisterMainHooks(r)
	ctxt, state, err := hook.NewContextFromEnvironment(r)
	if err != nil {
		fatalf("cannot create context: %v", err)
	}
	defer ctxt.Close()
	if err := hook.Main(r, ctxt, state); err != nil {
		fatalf("%v", err)
	}
}

func fatalf(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "runhook: %s\n", fmt.Sprintf(f, a...))
	os.Exit(1)
}
`))

type buildCharmParams struct {
	// pkg specifies the package that the hook will be built from.
	pkg *build.Package

	// charmDir specifies the destination directory to write
	// the charm files to.
	charmDir string

	// tempDir holds a temporary directory to use for
	// any temporary build artifacts.
	tempDir string

	// source specifies whether the source code should
	// be vendored into the charm.
	// This also implies that the hooks will have the
	// capability to recompile.
	source bool
}

type charmBuilder buildCharmParams

// buildCharm builds the runhook executable,
// and all the other charm pieces (hooks, metadata.yaml,
// config.yaml). It puts the runhook source file into goFile
// and the runhook executable into exe.
func buildCharm(p buildCharmParams) error {
	b := (*charmBuilder)(&p)
	code := generateCode(hookMainCode, b.pkg.ImportPath)
	var exe string
	if b.source {
		// Build the runhook executable anyway, just to be sure
		// that we can, but discard it.
		exe = filepath.Join(b.tempDir, "runhook")
	} else {
		exe = filepath.Join(b.charmDir, "bin", "runhook")
	}
	goFile := filepath.Join(b.charmDir, "src", "runhook", "runhook.go")
	if err := compile(goFile, exe, code, true); err != nil {
		return errors.Wrapf(err, "cannot build hooks main package")
	}
	if _, err := os.Stat(exe); err != nil {
		return errors.New("runhook command not built")
	}
	info, err := registeredCharmInfo(p.pkg.ImportPath, p.tempDir)
	if err != nil {
		return errors.Wrap(err)
	}
	if err := b.writeHooks(info.Hooks); err != nil {
		return errors.Wrapf(err, "cannot write hooks to charm")
	}
	if err := b.writeMeta(info.Relations); err != nil {
		return errors.Wrapf(err, "cannot write metadata.yaml")
	}
	if err := b.writeConfig(info.Config); err != nil {
		return errors.Wrapf(err, "cannot write config.yaml")
	}
	// Sanity check that the new config files parse correctly.
	_, err = charm.ReadCharmDir(b.charmDir)
	if err != nil {
		return errors.Wrapf(err, "charm will not read correctly; we've broken it, sorry")
	}
	if b.source {
		if err := b.vendorDeps(); err != nil {
			return errors.Wrapf(err, "cannot get dependencies")
		}
		if err := ioutil.WriteFile(filepath.Join(b.charmDir, "compile"), []byte(compileScript), 0755); err != nil {
			return errors.Wrap(err)
		}
	}
	return nil
}

// writeHooks ensures that the charm has the given set of hooks.
// TODO write install and start hooks even if they're not registered,
// because otherwise it won't be treated as a valid charm.
func (b *charmBuilder) writeHooks(hooks []string) error {
	if *verbose {
		log.Printf("writing hooks in %s", b.charmDir)
	}
	hookDir := filepath.Join(b.charmDir, "hooks")
	if err := os.MkdirAll(hookDir, 0777); err != nil {
		return errors.Wrapf(err, "failed to make hooks directory")
	}
	infos, err := ioutil.ReadDir(hookDir)
	if err != nil {
		return errors.Wrap(err)
	}
	if *verbose {
		log.Printf("found %d existing hooks", len(infos))
	}
	// Add any new hooks we need to the charm directory.
	for _, hookName := range hooks {
		hookPath := filepath.Join(hookDir, hookName)
		if *verbose {
			log.Printf("creating hook %s", hookPath)
		}
		if err := ioutil.WriteFile(hookPath, b.hookStub(hookName), 0755); err != nil {
			return errors.Wrap(err)
		}
	}
	return nil
}

// hookStubTemplate holds the template for the generated hook code.
// The apt-get flags are stolen from github.com/juju/utils/apt
var hookStubTemplate = template.Must(template.New("").Parse(`#!/bin/sh
set -ex
{{if eq .HookName "install"}}
apt-get '--option=Dpkg::Options::=--force-confold'  '--option=Dpkg::options::=--force-unsafe-io' --assume-yes --quiet install golang git mercurial

if test -e "$CHARM_DIR/bin/runhook"; then
	# the binary has been pre-compiled; no need to compile again.
	exit 0
fi
export GOPATH="$CHARM_DIR"
go get {{.GodepPath}}

"$CHARM_DIR/compile"
"$CHARM_DIR/bin/runhook" install
{{else if  .Source}}
if test -e "$CHARM_DIR/compile-always"; then
	"$CHARM_DIR/compile"
fi
{{end}}
$CHARM_DIR/bin/runhook {{.HookName}}
`))

type hookStubParams struct {
	Source    bool
	HookName  string
	GodepPath string
}

func (b *charmBuilder) hookStub(hookName string) []byte {
	return executeTemplate(hookStubTemplate, hookStubParams{
		Source:    b.source,
		HookName:  hookName,
		GodepPath: godepPath,
	})
}

func (b *charmBuilder) writeMeta(relations map[string]charm.Relation) error {
	metaFile, err := os.Open(filepath.Join(b.pkg.Dir, "metadata.yaml"))
	if err != nil {
		return errors.Wrap(err)
	}
	defer metaFile.Close()
	meta, err := charm.ReadMeta(metaFile)
	if err != nil {
		return errors.Wrapf(err, "cannot read metadata.yaml from %q", b.pkg.Dir)
	}
	// The metadata name must match the directory name otherwise
	// juju deploy will ignore the charm.
	meta.Name = filepath.Base(b.pkg.Dir)
	meta.Provides = make(map[string]charm.Relation)
	meta.Requires = make(map[string]charm.Relation)
	meta.Peers = make(map[string]charm.Relation)

	for name, rel := range relations {
		switch rel.Role {
		case charm.RoleProvider:
			meta.Provides[name] = rel
		case charm.RoleRequirer:
			meta.Requires[name] = rel
		case charm.RolePeer:
			meta.Peers[name] = rel
		default:
			return errors.Newf("unknown role %q in relation", rel.Role)
		}
	}
	if err := writeYAML(filepath.Join(b.charmDir, "metadata.yaml"), meta); err != nil {
		return errors.Wrapf(err, "cannot write metadata.yaml")
	}
	return nil
}

const yamlAutogenComment = "# " + autogenMessage + "\n"

func writeYAML(file string, val interface{}) error {
	data, err := yaml.Marshal(val)
	if err != nil {
		return errors.Wrapf(err, "cannot marshal YAML")
	}
	data = append([]byte(yamlAutogenComment), data...)
	if err := ioutil.WriteFile(file, data, 0666); err != nil {
		return errors.Wrap(err)
	}
	return nil
}

func (b *charmBuilder) writeConfig(config map[string]charm.Option) error {
	configPath := filepath.Join(b.charmDir, "config.yaml")
	if len(config) == 0 {
		return nil
	}
	if err := writeYAML(configPath, &charm.Config{
		Options: config,
	}); err != nil {
		return errors.Wrapf(err, "cannot write config.yaml")
	}
	return nil
}

var listSep = string(filepath.ListSeparator)

func (b *charmBuilder) vendorDeps() error {
	dir := filepath.Join(b.charmDir, "src", "runhook")
	// godep save requires the base package to be in a VCS, for
	// some odd reason, so we create one and then destroy it.
	gitCmd := runCmd(dir, nil, "git", "init")
	gitCmd.Stdout = nil // We don't want the chat.
	if err := gitCmd.Run(); err != nil {
		return errors.Wrapf(err, "cannot git init directory")
	}
	defer os.RemoveAll(filepath.Join(dir, ".git"))
	// We put the existing GOPATH at the start so that it doesn't matter that
	// we have already copied the charm's source code into $charmdir/src
	// and that it doesn't have an associated VCS.
	env := setenv(os.Environ(), "GOPATH="+os.Getenv("GOPATH")+listSep+b.charmDir)
	if err := runCmd(dir, env, "godep", "save").Run(); err != nil {
		if isExecNotFound(err) {
			return errors.Newf("godep executable not found; get it with: go get %s", godepPath)
		}
		return errors.Wrap(err)
	}
	return nil
}

func setenv(env []string, entry string) []string {
	i := strings.Index(entry, "=")
	if i == -1 {
		panic("no = in environment entry")
	}
	prefix := entry[0 : i+1]
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = entry
			return env
		}
	}
	return append(env, entry)
}

type templateParams struct {
	AutogenMessage string
	CharmPackage   string
	HookPackage    string
}

func generateCode(tmpl *template.Template, charmPackage string) []byte {
	return executeTemplate(tmpl, templateParams{
		CharmPackage:   charmPackage,
		HookPackage:    hookPackage,
		AutogenMessage: autogenMessage,
	})
}

func compile(goFile, exeFile string, mainCode []byte, crossCompile bool) error {
	env := os.Environ()
	if crossCompile {
		env = setenv(env, "CGOENABLED=false")
		env = setenv(env, "GOARCH=amd64")
		env = setenv(env, "GOOS=linux")
	}
	if err := os.MkdirAll(filepath.Dir(goFile), 0777); err != nil {
		return errors.Wrap(err)
	}
	if err := os.MkdirAll(filepath.Dir(exeFile), 0777); err != nil {
		return errors.Wrap(err)
	}
	if err := ioutil.WriteFile(goFile, mainCode, 0666); err != nil {
		return errors.Wrap(err)
	}
	if err := runCmd("", env, "go", "build", "-o", exeFile, goFile).Run(); err != nil {
		return errors.Wrapf(err, "failed to build")
	}
	return nil
}

func runCmd(dir string, env []string, cmd string, args ...string) *exec.Cmd {
	if *verbose {
		log.Printf("run %s %s", cmd, strings.Join(args, " "))
	}
	c := exec.Command(cmd, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = env
	c.Dir = dir
	return c
}

func isExecNotFound(err error) bool {
	e, ok := err.(*exec.Error)
	return ok && e.Err == exec.ErrNotFound
}

func executeTemplate(t *template.Template, param interface{}) []byte {
	var w bytes.Buffer
	if err := t.Execute(&w, param); err != nil {
		panic(err)
	}
	return w.Bytes()
}

var compileScript = `#!/bin/sh
set -e
if test -z "$CHARM_DIR"; then
	echo CHARM_DIR not set >&2
	exit 2
fi
export PATH="$CHARM_DIR/bin:$PATH"
cd "$CHARM_DIR/src/runhook"
export GOPATH="$CHARM_DIR:$(godep path)"
go install
`
