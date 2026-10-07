package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/Rampo0/infinite-memory/internal/config"
)

var agentName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func cmdAgents(args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	var err error
	switch {
	case args[0] == "list":
		listAgents()
	case args[0] == "add" && len(args) == 3:
		err = addAgent(args[1], args[2])
	case args[0] == "remove" && len(args) == 2:
		err = removeAgent(args[1])
	default:
		err = fmt.Errorf("usage: imem agents [list | add <name> <transcript dir> | remove <name>]")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func listAgents() {
	dir := config.AgentsDir()
	drops := config.ReadDropIns(dir)
	cfg := config.Load()
	for _, r := range cfg.AgentRoots {
		fmt.Printf("%-16s %s (config.json)\n", "-", r)
	}
	for _, d := range drops {
		fmt.Printf("%-16s %s\n", d.Name, d.Root)
	}
	if len(drops) == 0 && len(cfg.AgentRoots) == 0 {
		fmt.Printf("no agents registered (%s)\n", dir)
	}
}

func addAgent(name, root string) error {
	if !agentName.MatchString(name) {
		return fmt.Errorf("agent name %q: use lowercase letters, digits, - and _", name)
	}
	dir := config.AgentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config.AgentDropIn{Name: name, Root: root}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	fmt.Printf("registered %s → %s (%s)\n", name, root, path)
	return nil
}

func removeAgent(name string) error {
	if !agentName.MatchString(name) {
		return fmt.Errorf("agent name %q: use lowercase letters, digits, - and _", name)
	}
	err := os.Remove(filepath.Join(config.AgentsDir(), name+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
