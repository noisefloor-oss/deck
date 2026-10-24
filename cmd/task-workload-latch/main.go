// task-workload-latch is the inert child of pinned tini. After validating its
// bootstrap inputs it stops itself. Only an externally observed SIGCONT may
// advance it to the exact absolute provider executable.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) < 2 || !validProviderPath(os.Args[1]) {
		fmt.Fprintln(os.Stderr, "task-workload-latch: provider executable must be an immutable image path")
		os.Exit(64)
	}
	if err := validateEnvironment(os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "task-workload-latch: unsafe bootstrap environment")
		os.Exit(64)
	}
	if err := stopSelf(); err != nil {
		fmt.Fprintln(os.Stderr, "task-workload-latch: cannot enter inert state")
		os.Exit(70)
	}
	if err := syscall.Exec(os.Args[1], os.Args[1:], providerEnvironment(os.Environ())); err != nil {
		fmt.Fprintln(os.Stderr, "task-workload-latch: provider exec failed")
		os.Exit(126)
	}
}

func validateEnvironment(environment []string) error {
	allowed := map[string]struct{}{
		"HOME": {}, "PATH": {}, "TERM": {}, "NODE_VERSION": {}, "YARN_VERSION": {}, "HOSTNAME": {},
	}
	seen := map[string]struct{}{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		_, permitted := allowed[name]
		if !ok || !validName(name) || forbiddenName(name) || !permitted {
			return fmt.Errorf("unsafe environment")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate environment")
		}
		seen[name] = struct{}{}
		if (name == "HOME" && value != "/home/agent") || (name == "PATH" && value != "/usr/local/bin:/usr/bin:/bin") {
			return fmt.Errorf("unsafe environment value")
		}
		if (name == "NODE_VERSION" && value != "26.12.1") || (name == "YARN_VERSION" && value != "1.22.22") {
			return fmt.Errorf("unrecognized substrate environment value")
		}
		if name == "TERM" && value != "xterm-256color" || name == "HOSTNAME" && !validHostname(value) {
			return fmt.Errorf("unsafe environment value")
		}
	}
	if len(seen) != len(allowed) {
		return fmt.Errorf("incomplete environment")
	}
	return nil
}

func validHostname(hostname string) bool {
	if len(hostname) != len("ndq-")+32 || !strings.HasPrefix(hostname, "ndq-") {
		return false
	}
	for _, char := range hostname[len("ndq-"):] {
		if char < '0' || char > '9' && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validProviderPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path &&
		(strings.HasPrefix(path, "/usr/local/bin/") || strings.HasPrefix(path, "/opt/noisedeck/bin/"))
}

func validName(name string) bool {
	if name == "" || name[0] < 'A' || name[0] > 'Z' && name[0] != '_' {
		return false
	}
	for _, char := range name[1:] {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func forbiddenName(name string) bool {
	for _, prefix := range []string{"LD_", "DYLD_", "GIT_CONFIG", "GIT_SSH", "SSH_ASKPASS", "NOISEDECK_LATCH_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return name == "BASH_ENV" || name == "ENV" || name == "SHELLOPTS" || name == "BASHOPTS" || name == "GIT_EXEC_PATH" || name == "GIT_ASKPASS"
}

func providerEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "NOISEDECK_LATCH_") {
			result = append(result, entry)
		}
	}
	return result
}
