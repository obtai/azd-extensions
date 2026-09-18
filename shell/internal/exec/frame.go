// Package exec speaks the Container Apps exec protocol: the websocket
// `az containerapp exec` uses. The byte values come from azure-cli's
// containerapp/_ssh_utils.py; there is no published spec.
//
// Every message starts with a proxy byte. FORWARD carries a second, cluster
// byte naming the stream; INFO and ERROR carry text straight after the first.
package exec

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	proxyForward = 0
	proxyInfo    = 1
	proxyError   = 2

	clusterStdin  = 0
	clusterStdout = 1
	clusterStderr = 2
	clusterResize = 4
)

// Kind is what an inbound message is for.
type Kind int

const (
	Stdout Kind = iota
	Stderr
	Info
	Error
)

// stdinFrame wraps keystrokes for the container.
func stdinFrame(data []byte) []byte {
	return append([]byte{proxyForward, clusterStdin}, data...)
}

// resizeFrame tells the container's terminal its size. The JSON is spelt the
// way the az CLI spells it rather than through encoding/json, whose field
// names would need tags to match anyway.
func resizeFrame(width, height int) []byte {
	return append([]byte{proxyForward, clusterResize},
		fmt.Sprintf(`{"Width": %d, "Height": %d}`, width, height)...)
}

// decode splits an inbound message into what it is and its payload.
func decode(message []byte) (Kind, []byte, error) {
	if len(message) == 0 {
		return 0, nil, fmt.Errorf("empty message")
	}
	switch message[0] {
	case proxyInfo:
		return Info, message[1:], nil
	case proxyError:
		return Error, message[1:], nil
	case proxyForward:
		if len(message) < 2 {
			return 0, nil, fmt.Errorf("forwarded message with no stream byte")
		}
		switch message[1] {
		case clusterStdout:
			return Stdout, message[2:], nil
		case clusterStderr:
			return Stderr, message[2:], nil
		}
		return 0, nil, fmt.Errorf("unexpected stream byte %d", message[1])
	}
	return 0, nil, fmt.Errorf("unexpected proxy byte %d", message[0])
}

// Endpoint is the websocket URL for one container. Newer API versions return
// it on the replica as execEndpoint; older ones only give the log stream
// endpoint, whose host is the same proxy, so the az CLI's construction is the
// fallback.
func Endpoint(execEndpoint, logStreamEndpoint, subscription, resourceGroup, app, revision, replica, container, command string) (string, error) {
	if execEndpoint != "" {
		parsed, err := url.Parse(execEndpoint)
		if err != nil {
			return "", fmt.Errorf("parsing exec endpoint: %w", err)
		}
		if parsed.Scheme == "https" {
			parsed.Scheme = "wss"
		}
		values := parsed.Query()
		values.Set("command", command)
		parsed.RawQuery = values.Encode()
		return parsed.String(), nil
	}

	query := "command=" + url.QueryEscape(command)

	index := strings.Index(logStreamEndpoint, "/subscriptions/")
	if index < 0 {
		return "", fmt.Errorf("container %s has neither an exec nor a log stream endpoint", container)
	}
	host := strings.TrimPrefix(logStreamEndpoint[:index], "https://")
	return fmt.Sprintf(
		"wss://%s/subscriptions/%s/resourceGroups/%s/containerApps/%s/revisions/%s/replicas/%s/containers/%s/exec?%s",
		host, subscription, resourceGroup, app, revision, replica, container, query), nil
}
