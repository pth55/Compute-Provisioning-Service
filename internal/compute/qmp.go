package compute

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

type qmpMessage struct {
	QMP    json.RawMessage `json:"QMP"`
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
	Event string `json:"event"`
}

func qmpExecute(ctx context.Context, socket, command string, arguments map[string]any) (json.RawMessage, error) {
	if socket == "" {
		return nil, fmt.Errorf("QMP socket is unavailable; this may be a legacy VM created before QMP support")
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("connect to QMP socket %s: %w", socket, err)
	}
	defer conn.Close()
	timeout := 30 * time.Second
	if command == "human-monitor-command" {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	reader := bufio.NewReader(conn)

	// QMP greets every new connection before accepting commands.
	var greeting qmpMessage
	if err := readQMP(reader, &greeting); err != nil {
		return nil, fmt.Errorf("read QMP greeting: %w", err)
	}
	if len(greeting.QMP) == 0 {
		return nil, fmt.Errorf("QEMU returned an invalid QMP greeting")
	}
	if result, err := qmpRawCommand(conn, reader, "qmp_capabilities", nil); err != nil {
		return nil, fmt.Errorf("negotiate QMP capabilities: %w", err)
	} else {
		_ = result
	}
	result, err := qmpRawCommand(conn, reader, command, arguments)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func qmpRawCommand(conn net.Conn, reader *bufio.Reader, command string, arguments map[string]any) (json.RawMessage, error) {
	slog.Debug("QMP command started", "socket", "unix-socket", "command", command)
	payload := map[string]any{"execute": command}
	if len(arguments) > 0 {
		payload["arguments"] = arguments
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if _, err := conn.Write(encoded); err != nil {
		return nil, fmt.Errorf("send QMP command %s: %w", command, err)
	}
	for {
		var response qmpMessage
		if err := readQMP(reader, &response); err != nil {
			return nil, fmt.Errorf("read QMP response for %s: %w", command, err)
		}
		if response.Event != "" {
			continue
		} // Async QMP events can interleave with command responses.
		if response.Error != nil {
			return nil, fmt.Errorf("QMP command %s failed (%s): %s", command, response.Error.Class, response.Error.Desc)
		}
		if response.Return != nil {
			slog.Debug("QMP command completed", "command", command)
			return response.Return, nil
		}
	}
}

func readQMP(reader *bufio.Reader, target *qmpMessage) error {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, target); err != nil {
		return fmt.Errorf("decode QMP JSON: %w; payload=%q", err, strings.TrimSpace(string(line)))
	}
	return nil
}

func qmpStatus(ctx context.Context, socket string) (string, error) {
	payload, err := qmpExecute(ctx, socket, "query-status", nil)
	if err != nil {
		return "", err
	}
	var result struct {
		Status     string `json:"status"`
		Running    bool   `json:"running"`
		Singlestep bool   `json:"singlestep"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return "", fmt.Errorf("decode QMP query-status: %w", err)
	}
	if result.Status == "" {
		return "", fmt.Errorf("QMP query-status returned no status")
	}
	return result.Status, nil
}

func qmpHuman(ctx context.Context, socket, commandLine string) (string, error) {
	payload, err := qmpExecute(ctx, socket, "human-monitor-command", map[string]any{"command-line": commandLine})
	if err != nil {
		return "", err
	}
	var response string
	if err := json.Unmarshal(payload, &response); err != nil {
		return "", fmt.Errorf("decode HMP response: %w", err)
	}
	lower := strings.ToLower(response)
	if strings.Contains(lower, "error:") || strings.Contains(lower, "could not") || strings.Contains(lower, "failed") {
		return response, fmt.Errorf("QEMU monitor command %q returned: %s", commandLine, strings.TrimSpace(response))
	}
	return response, nil
}
