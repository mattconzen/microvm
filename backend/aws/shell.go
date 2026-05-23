package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/coder/websocket"
)

// AgentCore frames cap at 32 KB; keep a hair under to leave room for headers.
const shellFrameMax = 32 * 1024

// SigV4 service name for AgentCore data plane.
const shellSigningService = "bedrock-agentcore"

// runtimeSessionHeader is the sticky session header AgentCore uses to pin a
// request to a microVM. Lowercased to match HTTP/2 normalization.
const runtimeSessionHeader = "x-amzn-bedrock-agentcore-runtime-session-id"

// CredentialsProvider matches awssdk.CredentialsProvider — kept as a local
// alias so tests can fake it without dragging in the full SDK type.
type CredentialsProvider interface {
	Retrieve(ctx context.Context) (awssdk.Credentials, error)
}

// wsDialer abstracts websocket.Dial so tests can intercept the URL/headers
// without actually opening a connection.
type wsDialer func(ctx context.Context, urlStr string, opts *websocket.DialOptions) (wsConn, *http.Response, error)

// wsConn is the minimal Conn surface Shell uses; lets tests fake it.
type wsConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
}

// shellResize is the JSON control frame the agent recognises for window
// resize. cols/rows arrive as int because Starlette parses JSON loosely.
type shellResize struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// buildShellURL constructs the wss:// URL for the AgentCore data-plane
// WebSocket invocation endpoint. ARNs are URL-escaped so colons survive.
func buildShellURL(region, runtimeArn string) (string, error) {
	if region == "" {
		return "", errors.New("aws.shell: region is empty")
	}
	if runtimeArn == "" {
		return "", errors.New("aws.shell: runtime arn is empty")
	}
	u := &url.URL{
		Scheme: "wss",
		Host:   fmt.Sprintf("bedrock-agentcore.%s.amazonaws.com", region),
		Path:   fmt.Sprintf("/runtimes/%s/ws", runtimeArn),
	}
	return u.String(), nil
}

// signShellHandshake signs the WebSocket HTTP upgrade request with SigV4.
// We sign the empty body (no payload on a handshake) and inject SigV4
// headers into a copy that the dialer will merge with its own Connection /
// Upgrade / Sec-WebSocket-* headers.
func signShellHandshake(ctx context.Context, creds awssdk.Credentials, region, wssURL string, extra http.Header, now time.Time) (http.Header, error) {
	// Convert wss:// to https:// for the signer — the canonical request must
	// match what the AgentCore server validates after upgrading.
	signURL := wssURL
	if len(signURL) > 6 && signURL[:6] == "wss://" {
		signURL = "https://" + signURL[6:]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signURL, nil)
	if err != nil {
		return nil, fmt.Errorf("aws.shell: build sign request: %w", err)
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// Empty-body SHA256, hex-lowercase — required by SigV4.
	emptyHash := sha256.Sum256(nil)
	payloadHash := hex.EncodeToString(emptyHash[:])
	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, req, payloadHash, shellSigningService, region, now); err != nil {
		return nil, fmt.Errorf("aws.shell: sigv4 sign: %w", err)
	}
	return req.Header, nil
}

// shellWsDial is the default wsDialer; wraps coder/websocket.Dial.
var shellWsDial wsDialer = func(ctx context.Context, urlStr string, opts *websocket.DialOptions) (wsConn, *http.Response, error) {
	c, resp, err := websocket.Dial(ctx, urlStr, opts)
	if err != nil {
		return nil, resp, err
	}
	c.SetReadLimit(shellFrameMax + 4096) // a bit of slack for ws framing overhead
	return c, resp, nil
}

// chunkWrite splits p into shellFrameMax-byte slices and writes each as a
// single binary message. WebSocket fragmentation would also work, but
// AgentCore's 32 KB cap is per-frame, so chunking is the safer move.
func chunkWrite(ctx context.Context, c wsConn, p []byte) error {
	for len(p) > 0 {
		n := len(p)
		if n > shellFrameMax {
			n = shellFrameMax
		}
		if err := c.Write(ctx, websocket.MessageBinary, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// pumpStdinToWS reads tty.In and shovels chunks into the websocket. Returns
// when src EOFs, the context cancels, or the write errors.
func pumpStdinToWS(ctx context.Context, c wsConn, src io.Reader) error {
	buf := make([]byte, shellFrameMax)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if werr := chunkWrite(ctx, c, buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// pumpWSToStdout reads frames from the websocket and writes the payload to
// dst. Text frames are honoured (the agent may emit JSON control replies)
// but treated as raw bytes for the TTY pipe — the receiving terminal does
// the rendering.
func pumpWSToStdout(ctx context.Context, c wsConn, dst io.Writer) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, data, err := c.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure ||
				websocket.CloseStatus(err) == websocket.StatusGoingAway {
				return nil
			}
			return err
		}
		if _, werr := dst.Write(data); werr != nil {
			return werr
		}
	}
}

// runShell wires the two pump goroutines, the initial resize frame, and the
// teardown semantics into one place so the Backend.Shell method body stays
// readable. dial is parameterised for tests.
func runShell(ctx context.Context, dial wsDialer, region, runtimeArn, sessionID string, creds awssdk.Credentials, now time.Time, headers http.Header, in io.Reader, out io.Writer, cols, rows uint16) error {
	if out == nil {
		return errors.New("aws.shell: no output sink")
	}
	wssURL, err := buildShellURL(region, runtimeArn)
	if err != nil {
		return err
	}
	if headers == nil {
		headers = http.Header{}
	}
	if sessionID != "" {
		headers.Set(runtimeSessionHeader, sessionID)
	}
	signed, err := signShellHandshake(ctx, creds, region, wssURL, headers, now)
	if err != nil {
		return err
	}
	c, _, err := dial(ctx, wssURL, &websocket.DialOptions{HTTPHeader: signed})
	if err != nil {
		return fmt.Errorf("aws.shell: dial: %w", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "client closed")

	// Initial resize frame so the agent's PTY matches the user terminal.
	if cols > 0 || rows > 0 {
		payload, mErr := json.Marshal(shellResize{Type: "resize", Cols: cols, Rows: rows})
		if mErr != nil {
			return fmt.Errorf("aws.shell: marshal resize: %w", mErr)
		}
		if wErr := c.Write(ctx, websocket.MessageText, payload); wErr != nil {
			return fmt.Errorf("aws.shell: send resize: %w", wErr)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	record := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel()
	}

	if in != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record(pumpStdinToWS(ctx, c, in))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		record(pumpWSToStdout(ctx, c, out))
	}()
	wg.Wait()

	if errors.Is(firstErr, context.Canceled) || errors.Is(firstErr, io.EOF) {
		return nil
	}
	return firstErr
}
