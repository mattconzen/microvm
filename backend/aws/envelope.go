package aws

import (
	"encoding/base64"
	"encoding/json"

	"github.com/mattconzen/microvm/backend"
)

type Op string

const (
	OpExec      Op = "exec"
	OpPut       Op = "put"
	OpGet       Op = "get"
	OpSnapshot  Op = "snapshot"
	OpResume    Op = "resume"
	OpTerminate Op = "terminate"
)

type Request struct {
	Op        Op       `json:"op"`
	Cmd       []string `json:"cmd,omitempty"`
	Path      string   `json:"path,omitempty"`
	B64       string   `json:"b64,omitempty"`
	Name      string   `json:"name,omitempty"`
	Alias     string   `json:"alias,omitempty"`
	SnapID    string   `json:"snap_id,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	Locator   string   `json:"locator,omitempty"`
	SandboxID string   `json:"sandbox_id,omitempty"`
}

type ExecResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
	Error  string `json:"error,omitempty"`
}

type PutResponse struct {
	OK    bool   `json:"ok"`
	Bytes int64  `json:"bytes"`
	Error string `json:"error,omitempty"`
}

type GetResponse struct {
	B64   string `json:"b64"`
	Bytes int64  `json:"bytes"`
	Error string `json:"error,omitempty"`
}

type SnapshotResponse struct {
	Alias   string `json:"alias"`
	Name    string `json:"name,omitempty"`
	Locator string `json:"locator,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Error   string `json:"error,omitempty"`
}

type ResumeResponse struct {
	Alias string `json:"alias"`
	Error string `json:"error,omitempty"`
}

type TerminateResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func ExecRequest(cmd []string) ([]byte, error) {
	return json.Marshal(Request{Op: OpExec, Cmd: cmd})
}

func PutRequest(path string, data []byte) ([]byte, error) {
	return json.Marshal(Request{
		Op:   OpPut,
		Path: path,
		B64:  base64.StdEncoding.EncodeToString(data),
	})
}

func GetRequest(path string) ([]byte, error) {
	return json.Marshal(Request{Op: OpGet, Path: path})
}

func SnapshotRequest(spec backend.SnapshotSpec, mode string) ([]byte, error) {
	return json.Marshal(Request{
		Op:     OpSnapshot,
		Name:   spec.Name,
		SnapID: spec.ID,
		Mode:   mode,
	})
}

func ResumeRequest(alias, locator, mode string) ([]byte, error) {
	return json.Marshal(Request{
		Op:      OpResume,
		Alias:   alias,
		Locator: locator,
		Mode:    mode,
	})
}

func TerminateRequest() ([]byte, error) {
	return json.Marshal(Request{Op: OpTerminate})
}

func DecodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// injectSandboxID decodes an already-marshaled request payload, sets the
// SandboxID field, and re-encodes it. EFS mode needs the shellagent to know
// which subdir to operate on, but the sandbox id isn't known to the request
// constructors — they only see the spec/cmd. Callers with no sandbox id (e.g.
// the Resume path that re-targets a snapshot's session) pass an empty string,
// in which case the payload is returned unchanged so the wire shape stays
// identical to the pre-EFS world.
func injectSandboxID(payload []byte, sandboxID string) ([]byte, error) {
	if sandboxID == "" {
		return payload, nil
	}
	var req Request
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	req.SandboxID = sandboxID
	return json.Marshal(req)
}
