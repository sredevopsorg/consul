package main

import (
	"fmt"
	"github.com/mitchellh/go-homedir"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"syscall"

	mcli "github.com/mitchellh/cli"

	"github.com/hashicorp/consul/command"
	"github.com/hashicorp/consul/command/cli"
	"github.com/hashicorp/consul/command/version"
	"github.com/hashicorp/consul/lib"
	_ "github.com/hashicorp/consul/service_os"
	cliversion "github.com/hashicorp/consul/version"
)

func init() {
	lib.SeedMathRand()
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	log.SetOutput(ioutil.Discard)

	ui := &cli.BasicUI{
		BasicUi: mcli.BasicUi{Writer: os.Stdout, ErrorWriter: os.Stderr},
	}
	cmds := command.CommandsFromRegistry(ui)
	var names []string
	for c := range cmds {
		names = append(names, c)
	}

	cli := &mcli.CLI{
		Args:         os.Args[1:],
		Commands:     cmds,
		Autocomplete: true,
		Name:         "consul",
		HelpFunc:     mcli.FilteredHelpFunc(names, mcli.BasicHelpFunc("consul")),
		HelpWriter:   os.Stdout,
		ErrorWriter:  os.Stderr,
		MissingCommandFunc: func(command string, args []string) (bool, error) {
			if len(args) == 0 {
				return false, nil
			}

			home, err := homedir.Dir()
			if err != nil {
				return false, err
			}

			pluginBinary := fmt.Sprintf("consul-%s", args[0])
			pluginPath := filepath.Join(home, ".consul", "plugins", pluginBinary)
			_, err = os.Stat(pluginPath)
			if err != nil {
				return false, err
			}

			env := os.Environ()
			env = append(env, fmt.Sprintf("CONSUL_CLI_VERSION=%s", cliversion.Version))
			execErr := syscall.Exec(pluginPath, append([]string{pluginBinary}, args[1:]...), env)
			if execErr != nil {
				return false, err
			}
			return true, nil
		},
	}

	if cli.IsVersion() {
		cmd := version.New(ui)
		return cmd.Run(nil)
	}

	exitCode, err := cli.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing CLI: %v\n", err)
		return 1
	}

	return exitCode
}
