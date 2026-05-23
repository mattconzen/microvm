package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildShellURL(t *testing.T) {
	cases := []struct {
		name   string
		region string
		arn    string
		want   string
		err    bool
	}{
		{
			name:   "happy path us-east-1",
			region: "us-east-1",
			arn:    "arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell",
			want:   "wss://bedrock-agentcore.us-east-1.amazonaws.com/runtimes/arn:aws:bedrock-agentcore:us-east-1:123:runtime/microvm-shell/ws",
		},
		{
			name:   "happy path eu-west-2",
			region: "eu-west-2",
			arn:    "arn:aws:bedrock-agentcore:eu-west-2:999:runtime/x",
			want:   "wss://bedrock-agentcore.eu-west-2.amazonaws.com/runtimes/arn:aws:bedrock-agentcore:eu-west-2:999:runtime/x/ws",
		},
		{
			name:   "empty region",
			region: "",
			arn:    "arn:aws:bedrock-agentcore:eu-west-2:999:runtime/x",
			err:    true,
		},
		{
			name:   "empty arn",
			region: "us-east-1",
			arn:    "",
			err:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildShellURL(tc.region, tc.arn)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(got, "wss://"), "scheme must be wss, got %s", got)
			assert.Contains(t, got, "bedrock-agentcore."+tc.region+".amazonaws.com")
			assert.True(t, strings.HasSuffix(got, "/ws"), "path must end /ws, got %s", got)
		})
	}
}

func TestSignShellHandshakeAddsAuthorization(t *testing.T) {
	creds := awssdk.Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "secret",
		SessionToken:    "session",
	}
	hdr, err := signShellHandshake(context.Background(), creds, "us-east-1",
		"wss://bedrock-agentcore.us-east-1.amazonaws.com/runtimes/arn:aws:bedrock-agentcore:us-east-1:123:runtime/x/ws",
		http.Header{runtimeSessionHeader: []string{"sess-1"}},
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.NotEmpty(t, hdr.Get("Authorization"), "Authorization header must be set")
	assert.Contains(t, hdr.Get("Authorization"), "AWS4-HMAC-SHA256")
	assert.Equal(t, "sess-1", hdr.Get(runtimeSessionHeader))
	assert.NotEmpty(t, hdr.Get("X-Amz-Date"))
	assert.Equal(t, creds.SessionToken, hdr.Get("X-Amz-Security-Token"))
}

// fakeConn implements wsConn for runShell tests.
type fakeConn struct {
	mu       sync.Mutex
	writes   []writeRec
	reads    chan readRec
	closeErr error
	closed   bool
}

type writeRec struct {
	typ  websocket.MessageType
	data []byte
}

type readRec struct {
	typ  websocket.MessageType
	data []byte
	err  error
}

func newFakeConn() *fakeConn {
	return &fakeConn{reads: make(chan readRec, 16)}
}

func (f *fakeConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case r, ok := <-f.reads:
		if !ok {
			return 0, nil, io.EOF
		}
		return r.typ, r.data, r.err
	}
}

func (f *fakeConn) Write(_ context.Context, typ websocket.MessageType, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Copy because callers may reuse p.
	buf := make([]byte, len(p))
	copy(buf, p)
	f.writes = append(f.writes, writeRec{typ: typ, data: buf})
	return nil
}

func (f *fakeConn) Close(_ websocket.StatusCode, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.reads)
	}
	return f.closeErr
}

func (f *fakeConn) writesSnapshot() []writeRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]writeRec, len(f.writes))
	copy(out, f.writes)
	return out
}

func TestRunShellSendsResizeAndPumpsBothWays(t *testing.T) {
	fc := newFakeConn()
	var capturedURL string
	var capturedHeader http.Header
	dial := func(_ context.Context, urlStr string, opts *websocket.DialOptions) (wsConn, *http.Response, error) {
		capturedURL = urlStr
		if opts != nil {
			capturedHeader = opts.HTTPHeader.Clone()
		}
		return fc, nil, nil
	}
	// Queue some agent->client output, then EOF.
	fc.reads <- readRec{typ: websocket.MessageBinary, data: []byte("hello from agent\n")}
	go func() {
		// Let the read pump consume, then close to terminate.
		time.Sleep(20 * time.Millisecond)
		fc.Close(websocket.StatusNormalClosure, "")
	}()

	creds := awssdk.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "s"}
	in := bytes.NewReader([]byte("ls -la\n"))
	var out bytes.Buffer
	err := runShell(context.Background(), dial,
		"us-west-2",
		"arn:aws:bedrock-agentcore:us-west-2:123:runtime/microvm-shell",
		"sess-xyz",
		creds, time.Now(), nil, in, &out, 132, 50)
	require.NoError(t, err)
	assert.Equal(t, "hello from agent\n", out.String())

	assert.True(t, strings.HasPrefix(capturedURL, "wss://bedrock-agentcore.us-west-2.amazonaws.com/runtimes/"), "url: %s", capturedURL)
	assert.True(t, strings.HasSuffix(capturedURL, "/ws"))
	require.NotNil(t, capturedHeader)
	assert.Equal(t, "sess-xyz", capturedHeader.Get(runtimeSessionHeader))
	assert.Contains(t, capturedHeader.Get("Authorization"), "AWS4-HMAC-SHA256")

	writes := fc.writesSnapshot()
	require.GreaterOrEqual(t, len(writes), 2, "expected resize + stdin frames")
	// First frame must be the resize JSON.
	assert.Equal(t, websocket.MessageText, writes[0].typ)
	var rz shellResize
	require.NoError(t, json.Unmarshal(writes[0].data, &rz))
	assert.Equal(t, "resize", rz.Type)
	assert.Equal(t, uint16(132), rz.Cols)
	assert.Equal(t, uint16(50), rz.Rows)

	// Remaining frames carry the stdin bytes (joined).
	var stdinBytes []byte
	for _, w := range writes[1:] {
		assert.Equal(t, websocket.MessageBinary, w.typ)
		stdinBytes = append(stdinBytes, w.data...)
	}
	assert.Equal(t, []byte("ls -la\n"), stdinBytes)
}

func TestRunShellChunksLargeStdin(t *testing.T) {
	fc := newFakeConn()
	dial := func(_ context.Context, _ string, _ *websocket.DialOptions) (wsConn, *http.Response, error) {
		return fc, nil, nil
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		fc.Close(websocket.StatusNormalClosure, "")
	}()
	creds := awssdk.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "s"}
	// 100 KB of input — must be split across multiple frames given the 32 KB cap.
	big := bytes.Repeat([]byte("x"), 100*1024)
	in := bytes.NewReader(big)
	var out bytes.Buffer
	err := runShell(context.Background(), dial,
		"us-east-1",
		"arn:aws:bedrock-agentcore:us-east-1:123:runtime/x",
		"sess",
		creds, time.Now(), nil, in, &out, 0, 0)
	require.NoError(t, err)
	writes := fc.writesSnapshot()
	for _, w := range writes {
		assert.LessOrEqual(t, len(w.data), shellFrameMax, "frame exceeds 32 KB cap")
	}
}

func TestRunShellRequiresOutSink(t *testing.T) {
	dial := func(_ context.Context, _ string, _ *websocket.DialOptions) (wsConn, *http.Response, error) {
		return nil, nil, errors.New("should not dial")
	}
	creds := awssdk.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "s"}
	err := runShell(context.Background(), dial,
		"us-east-1", "arn:x", "sess", creds, time.Now(), nil, nil, nil, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no output sink")
}
