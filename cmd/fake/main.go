// Command fake stands in for a vendor CLI in eval trials. It prints a device
// login screen, accepts any code, and records the login in a marker file that
// the fake harness auth probe reads. "fake connector" is a trebi-connector/1
// sandbox adapter, configured by TREBI_FAKEADAPTER.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			os.Exit(login())
		case "logout":
			os.Exit(logout())
		case "status":
			os.Exit(status())
		case "chat":
			os.Exit(chat())
		case "connector":
			os.Exit(fakeadapter.Main())
		}
	}
	os.Exit(chat())
}

// markerPath is the credential file, under the same harness directory the
// trebi auth probe reads. TREBI_HOME keeps a trial out of the real home.
func markerPath() string {
	base := os.Getenv("TREBI_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".trebi")
	}
	return filepath.Join(base, "harness", "fake", "auth.json")
}

func login() int {
	fmt.Println("To authenticate, visit:")
	fmt.Println()
	fmt.Println("https://example.invalid/fake/device")
	fmt.Println()
	fmt.Println("Enter code: FAKE-0000")
	os.Stdout.Sync()
	code, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(code) == "" {
		fmt.Println("login failed: no code")
		return 1
	}
	path := markerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Println("login failed:", err)
		return 1
	}
	body := fmt.Sprintf("{\"code\":%q,\"logged_in\":true}\n", strings.TrimSpace(code))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		fmt.Println("login failed:", err)
		return 1
	}
	fmt.Println("Logged in.")
	return 0
}

func logout() int {
	if err := os.Remove(markerPath()); err != nil && !os.IsNotExist(err) {
		fmt.Println("logout failed:", err)
		return 1
	}
	fmt.Println("Logged out.")
	return 0
}

func chat() int {
	fmt.Println("fake: ok")
	path := transcriptPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Println("chat failed:", err)
		return 1
	}
	body := "{\"role\":\"assistant\",\"text\":\"fake: ok\"}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		fmt.Println("chat failed:", err)
		return 1
	}
	return 0
}

func transcriptPath() string {
	return filepath.Join(filepath.Dir(markerPath()), "transcript.jsonl")
}

func status() int {
	if _, err := os.Stat(markerPath()); err != nil {
		fmt.Println("Not logged in.")
		return 1
	}
	fmt.Println("Logged in.")
	return 0
}
