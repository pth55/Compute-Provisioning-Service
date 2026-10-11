package cloudinit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Build produces cloud-init user-data. If userData starts with #cloud-config it
// is treated as YAML. Otherwise it is passed to the guest's shell as one command.
// JSON strings are valid YAML double-quoted scalars and safely escape shell text.
func Build(userData, publicKey string) ([]byte, error) {
	key := strings.TrimSpace(publicKey)
	if userData != "" && strings.HasPrefix(strings.TrimSpace(userData), "#cloud-config") {
		config := strings.TrimSpace(userData) + "\n"
		if !strings.Contains(config, "ssh_pwauth:") {
			config = appendTopLevel(config, "ssh_pwauth: false")
		}
		if key != "" {
			var err error
			config, err = addAuthorizedKey(config, key)
			if err != nil {
				return nil, err
			}
		}
		return []byte(config), nil
	}

	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_pwauth: false\n")
	b.WriteString("disable_root: true\n")
	if key != "" {
		quoted, _ := json.Marshal(key)
		b.WriteString("ssh_authorized_keys:\n  - ")
		b.WriteString(string(quoted))
		b.WriteByte('\n')
	}
	if strings.TrimSpace(userData) != "" {
		quoted, err := json.Marshal(userData)
		if err != nil {
			return nil, fmt.Errorf("encode user-data command: %w", err)
		}
		b.WriteString("runcmd:\n  - [\"/bin/sh\", \"-lc\", ")
		b.WriteString(string(quoted))
		b.WriteString("]\n")
	}
	return []byte(b.String()), nil
}

func appendTopLevel(config, line string) string {
	config = strings.TrimRight(config, "\n")
	return config + "\n" + line + "\n"
}

// addAuthorizedKey updates the block-list form of ssh_authorized_keys without
// adding a duplicate YAML mapping key. Inline key lists are rejected explicitly
// instead of silently ignoring the API's ssh_public_key field.
func addAuthorizedKey(config, key string) (string, error) {
	quoted, _ := json.Marshal(key)
	lines := strings.Split(strings.TrimRight(config, "\n"), "\n")
	section := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "ssh_authorized_keys:" && strings.TrimLeft(line, " \t") == line {
			section = i
			break
		}
		if strings.HasPrefix(line, "ssh_authorized_keys:") && strings.TrimSpace(strings.TrimPrefix(line, "ssh_authorized_keys:")) != "" {
			return "", fmt.Errorf("user_data uses inline ssh_authorized_keys; use a YAML block list so CPS can append ssh_public_key")
		}
	}
	if section < 0 {
		lines = append(lines, "ssh_authorized_keys:", "  - "+string(quoted))
		return strings.Join(lines, "\n") + "\n", nil
	}
	for i := section + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && !strings.HasPrefix(line, "#") {
			break
		}
		if strings.Contains(line, key) {
			return strings.Join(lines, "\n") + "\n", nil
		}
	}
	insertAt := len(lines)
	for i := section + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			insertAt = i
			break
		}
	}
	lines = append(lines, "")
	copy(lines[insertAt+1:], lines[insertAt:])
	lines[insertAt] = "  - " + string(quoted)
	return strings.Join(lines, "\n") + "\n", nil
}
