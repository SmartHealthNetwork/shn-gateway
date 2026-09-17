package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

var controlledPrefix = []string{"--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher"}
var ownedSuffix = []string{"--server.address=127.0.0.1", "--server.port=18080"}

func controlledAddress(args []string) (string, error) {
	if len(args) < 6 || !reflect.DeepEqual(args[:4], controlledPrefix) || !reflect.DeepEqual(args[len(args)-2:], ownedSuffix) {
		return "", fmt.Errorf("controlled child requires actual owned Java launch")
	}
	return "127.0.0.1:18080", nil
}

// observeRuntime is an explicitly mounted verifier utility, not an image command.
// It reads actual child argv and socket ownership from both Linux socket tables.
func observeRuntime() error {
	read := func(path string) (string, error) { b, e := os.ReadFile(path); return string(b), e }
	children, err := childPIDs("/proc/1/task")
	if err != nil {
		return err
	}
	var pid int
	var argv []string
	for _, child := range children {
		raw, e := read(fmt.Sprintf("/proc/%d/cmdline", child))
		if e != nil {
			return e
		}
		args := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
		if len(args) >= 5 && reflect.DeepEqual(args[1:5], controlledPrefix) {
			if pid != 0 {
				return fmt.Errorf("multiple Java children")
			}
			pid = child
			argv = args
		}
	}
	if pid <= 1 {
		return fmt.Errorf("owned Java child unavailable")
	}
	sockets := func(process int) ([]string, error) {
		entries, e := os.ReadDir(fmt.Sprintf("/proc/%d/fd", process))
		if e != nil {
			return nil, e
		}
		out := []string{}
		for _, entry := range entries {
			target, e := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", process, entry.Name()))
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return nil, e
			}
			if strings.HasPrefix(target, "socket:[") {
				out = append(out, strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]"))
			}
		}
		return out, nil
	}
	parentSockets, err := sockets(1)
	if err != nil {
		return err
	}
	childSockets, err := sockets(pid)
	if err != nil {
		return err
	}
	tcp, err := read("/proc/net/tcp")
	if err != nil {
		return err
	}
	tcp6, err := read("/proc/net/tcp6")
	if err != nil {
		return err
	}
	env, err := read(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return err
	}
	executable, err := runtimeExecutable(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"child_pid": pid, "argv": argv, "env": strings.Split(strings.TrimSuffix(env, "\x00"), "\x00"), "executable": executable, "parent_sockets": parentSockets, "child_sockets": childSockets, "tcp": tcp, "tcp6": tcp6})
}

func runtimeExecutable(path string) (string, error) {
	// Preserve the kernel's target, even when it is outside this filesystem or
	// names a deleted executable. Resolving it changes the observed evidence.
	return os.Readlink(path)
}

// Linux children files belong to individual tasks. Go may exec from any OS
// thread, so reading only task/1 would miss a live child of another PID1 thread.
func childPIDs(taskRoot string) ([]int, error) {
	tasks, err := os.ReadDir(taskRoot)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	for _, task := range tasks {
		if !task.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(taskRoot, task.Name(), "children"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, word := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(word)
			if err != nil || pid <= 1 {
				return nil, fmt.Errorf("invalid child PID evidence")
			}
			seen[pid] = true
		}
	}
	children := make([]int, 0, len(seen))
	for pid := range seen {
		children = append(children, pid)
	}
	sort.Ints(children)
	return children, nil
}
