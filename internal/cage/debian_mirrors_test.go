package cage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDebianMirrorFailureFallbackAndPreference(t *testing.T) {
	old := "https://mirrors.nju.edu.cn/debian\tpriority:1\nhttps://mirrors.ustc.edu.cn/debian\tpriority:2\nhttps://deb.debian.org/debian\tpriority:3\n"
	for _, mode := range []string{"reachable", "bookworm", "tls-failure", "official-failure", "nju-failure", "official", "custom", "old", "reboot", "invalid", "nju", "ustc", "ustc-nju-failure", "host-ustc", "host-invalid"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			mirrors := filepath.Join(dir, "mirrors")
			os.MkdirAll(mirrors, 0755)
			list := filepath.Join(mirrors, "debian.list")
			content := "https://deb.debian.org/debian\n"
			if mode == "custom" {
				content = "https://custom.invalid/debian\n"
			}
			if mode == "old" {
				content = old
			}
			if mode == "reboot" {
				content = "# Cage managed Debian mirrors\nhttps://deb.debian.org/debian\tpriority:1\n"
			}
			os.WriteFile(list, []byte(content), 0644)
			os.WriteFile(filepath.Join(mirrors, "debian-security.list"), []byte("https://deb.debian.org/debian-security\n"), 0644)
			preference := filepath.Join(dir, "preference")
			switch mode {
			case "official", "invalid", "nju", "ustc":
				os.WriteFile(preference, []byte(mode+"\n"), 0644)
			case "ustc-nju-failure":
				os.WriteFile(preference, []byte("ustc\n"), 0644)
			case "host-ustc":
				// The host's choice replaces an older saved preference.
				os.WriteFile(preference, []byte("official\n"), 0644)
			}
			for name, body := range map[string]string{
				"sudo": "#!/bin/sh\nexec \"$@\"\n",
				"apt-helper": `#!/bin/sh
printf '%s\n' "$*" >> "$FIXTURE/probes"
[ "$MODE" != tls-failure ] || exit 1
case "$*" in
 *deb.debian.org*) [ "$MODE" != nju-failure ] && [ "$MODE" != official-failure ] || exit 1;;
 *mirrors.nju*) [ "$MODE" != nju-failure ] && [ "$MODE" != ustc-nju-failure ] || exit 1;;
esac
exit 0
`,
				"timeout": "#!/bin/sh\nshift\nexec \"$@\"\n",
			} {
				os.WriteFile(filepath.Join(dir, name), []byte(body), 0755)
			}
			script := strings.ReplaceAll(DebianMirrorsScript, "/etc/apt/mirrors", mirrors)
			script = strings.ReplaceAll(script, "/etc/apt/sources.list.d", filepath.Join(dir, "sources.list.d"))
			script = strings.ReplaceAll(script, "/etc/apt/apt.conf.d", filepath.Join(dir, "apt.conf.d"))
			script = strings.ReplaceAll(script, "/etc/claude-cage/debian-binary-only", filepath.Join(dir, "binary-only"))
			script = strings.ReplaceAll(script, "/etc/claude-cage/debian-mirror", preference)
			script = strings.ReplaceAll(script, "/usr/lib/apt/apt-helper", filepath.Join(dir, "apt-helper"))
			suite := "trixie"
			if mode == "bookworm" {
				suite = "bookworm"
			}
			osRelease := filepath.Join(dir, "os-release")
			if err := os.WriteFile(osRelease, []byte("VERSION_CODENAME="+suite+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			script = strings.ReplaceAll(script, "/etc/os-release", osRelease)
			run := func(choice string) ([]byte, error) {
				cmd := exec.Command("sh", "-c", choice+script)
				cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FIXTURE="+dir, "MODE="+mode)
				return cmd.CombinedOutput()
			}
			first := ""
			switch mode {
			case "host-ustc":
				first = debianMirrorChoice("ustc")
			case "host-invalid":
				// Go never emits an unknown value; the guest script still rejects one.
				first = "debian_mirror_choice=fastest\n"
			}
			out, err := run(first)
			if mode == "invalid" || mode == "host-invalid" {
				if err == nil {
					t.Fatal("invalid preference accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			data, _ := os.ReadFile(list)
			got := string(data)
			switch mode {
			case "custom":
				if got != content {
					t.Fatal("custom source overwritten")
				}
			case "official", "tls-failure", "reachable", "bookworm", "old", "reboot":
				if strings.Contains(got, "mirrors.") || !strings.Contains(got, "deb.debian.org") {
					t.Fatal(got)
				}
			case "nju-failure":
				if strings.Contains(got, "mirrors.nju") || !strings.Contains(got, "mirrors.ustc") {
					t.Fatal(got)
				}
			case "ustc", "host-ustc":
				// The mirror measured fastest comes first, not a fixed order.
				if got != "# Cage managed Debian mirrors\nhttps://mirrors.ustc.edu.cn/debian\tpriority:1\nhttps://mirrors.nju.edu.cn/debian\tpriority:2\nhttps://deb.debian.org/debian\tpriority:3\n" {
					t.Fatal(got)
				}
			case "ustc-nju-failure":
				if got != "# Cage managed Debian mirrors\nhttps://mirrors.ustc.edu.cn/debian\tpriority:1\nhttps://deb.debian.org/debian\tpriority:2\n" {
					t.Fatal(got)
				}
			default:
				// official-failure and nju: nju, ustc, then the official source.
				if got != "# Cage managed Debian mirrors\n"+old {
					t.Fatal(got)
				}
			}
			if mode == "nju" || mode == "ustc" || mode == "host-ustc" {
				if probes, _ := os.ReadFile(filepath.Join(dir, "probes")); strings.Contains(string(probes), "deb.debian.org") {
					t.Fatal("domestic preference probed the official source first")
				}
			}
			if mode == "host-ustc" {
				// The host's measured choice is kept for later runs without it.
				if saved, _ := os.ReadFile(preference); strings.TrimSpace(string(saved)) != "ustc" {
					t.Fatalf("host choice not persisted: %q", saved)
				}
			}
			if mode == "official" {
				if probes, _ := os.ReadFile(filepath.Join(dir, "probes")); len(probes) > 0 {
					t.Fatal("official preference contacted mirrors")
				}
			}
			if mode == "reachable" || mode == "bookworm" {
				probes, err := os.ReadFile(filepath.Join(dir, "probes"))
				if err != nil || !strings.Contains(string(probes), "/dists/"+suite+"/InRelease") || !strings.Contains(string(probes), "/dists/"+suite+"-security/InRelease") {
					t.Fatalf("wrong distribution probes: %s, %v", probes, err)
				}
				if strings.Contains(string(probes), "mirrors.") {
					t.Fatal("reachable official source still contacted domestic mirrors")
				}
			}
			if out, err := run(""); err != nil {
				t.Fatalf("repeat: %v %s", err, out)
			}
			after, _ := os.ReadFile(list)
			if string(after) != got {
				t.Fatal("preference changed on repeated setup")
			}
		})
	}
}

// The host's choice reaches only the root shell that rewrites the sources.
// It must not become an exported variable that later installers inherit.
func TestDebianPrepareScriptKeepsChoiceOutOfEnvironment(t *testing.T) {
	if DebianPrepareScriptFor("fastest") != DebianPrepareScript || strings.Contains(DebianPrepareScript, "debian_mirror_choice=") {
		t.Fatal("unknown or empty preference reached the guest")
	}
	script := DebianPrepareScriptFor("ustc")
	if !strings.Contains(script, "\ndebian_mirror_choice=ustc\n") || strings.Contains(script, "export debian_mirror_choice") || strings.Contains(script, DebianMirrorEnv+"=") {
		t.Fatalf("choice not passed as a plain shell variable: %q", script)
	}
}

func TestDebianSourcesSkipSourceIndexesAndTranslations(t *testing.T) {
	const stock = "Types: deb deb-src\nURIs: mirror+file://%s/debian.list\nSuites: trixie trixie-updates trixie-backports\n\n" +
		"Types: deb deb-src\nURIs: mirror+file://%s/debian-security.list\nSuites: trixie-security\n"
	for _, mode := range []string{"stock", "custom", "edited-later"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			mirrors := filepath.Join(dir, "mirrors")
			sourcesDir := filepath.Join(dir, "sources.list.d")
			conf := filepath.Join(dir, "apt.conf.d")
			for _, d := range []string{mirrors, sourcesDir, conf} {
				if err := os.MkdirAll(d, 0755); err != nil {
					t.Fatal(err)
				}
			}
			os.WriteFile(filepath.Join(mirrors, "debian.list"), []byte("https://deb.debian.org/debian\n"), 0644)
			os.WriteFile(filepath.Join(mirrors, "debian-security.list"), []byte("https://deb.debian.org/debian-security\n"), 0644)
			preference := filepath.Join(dir, "preference")
			os.WriteFile(preference, []byte("official\n"), 0644)
			sources := filepath.Join(sourcesDir, "debian.sources")
			content := strings.ReplaceAll(stock, "%s", mirrors)
			if mode == "custom" {
				content = "Types: deb deb-src\nURIs: https://custom.invalid/debian\nSuites: trixie\n"
			}
			os.WriteFile(sources, []byte(content), 0644)
			os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\nexec \"$@\"\n"), 0755)
			script := strings.ReplaceAll(DebianMirrorsScript, "/etc/apt/mirrors", mirrors)
			script = strings.ReplaceAll(script, "/etc/apt/sources.list.d", sourcesDir)
			script = strings.ReplaceAll(script, "/etc/apt/apt.conf.d", conf)
			script = strings.ReplaceAll(script, "/etc/claude-cage/debian-binary-only", filepath.Join(dir, "binary-only"))
			script = strings.ReplaceAll(script, "/etc/claude-cage/debian-mirror", preference)
			osRelease := filepath.Join(dir, "os-release")
			os.WriteFile(osRelease, []byte("VERSION_CODENAME=trixie\n"), 0644)
			script = strings.ReplaceAll(script, "/etc/os-release", osRelease)
			run := func() {
				t.Helper()
				cmd := exec.Command("sh", "-c", script)
				cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			run()
			data, _ := os.ReadFile(sources)
			languages := filepath.Join(conf, "50cage-no-translations")
			_, confErr := os.Stat(languages)
			if mode == "custom" {
				if string(data) != content || confErr == nil {
					t.Fatalf("changed a custom source list: %q, conf written %v", data, confErr == nil)
				}
				return
			}
			if strings.Contains(string(data), "deb-src") || strings.Count(string(data), "Types: deb\n") != 2 || !strings.Contains(string(data), "Suites: trixie-security") {
				t.Fatalf("stock list not trimmed: %q", data)
			}
			if conf, err := os.ReadFile(languages); err != nil || !strings.Contains(string(conf), `Acquire::Languages "none";`) {
				t.Fatalf("translations still on: %q %v", conf, err)
			}
			if mode == "edited-later" {
				// The user turns translations and source indexes back on; a
				// later maintenance run leaves both alone.
				os.Remove(languages)
				restored := strings.ReplaceAll(string(data), "Types: deb\n", "Types: deb deb-src\n")
				os.WriteFile(sources, []byte(restored), 0644)
				run()
				if data, _ := os.ReadFile(sources); string(data) != restored {
					t.Fatalf("a later run undid the user's source types: %q", data)
				}
				if _, err := os.Stat(languages); err == nil {
					t.Fatal("a later run turned translations off again")
				}
			}
		})
	}
}
