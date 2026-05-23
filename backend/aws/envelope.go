package aws

import (
	"encoding/base64"
	"encoding/json"
)

type Op string

const (
	OpExec      Op = "exec"
	OpPut       Op = "put"
	OpGet       Op = "get"
	OpShell     Op = "shell"
	OpSnapshot  Op = "snapshot"
	OpResume    Op = "resume"
	OpTerminate Op = "terminate"
)

type Request struct {
	Op    Op       `json:"op"`
	Cmd   []string `json:"cmd,omitempty"`
	Path  string   `json:"path,omitempty"`
	B64   string   `json:"b64,omitempty"`
	TTY   bool     `json:"tty,omitempty"`
	Cols  uint16   `json:"cols,omitempty"`
	Rows  uint16   `json:"rows,omitempty"`
	Name  string   `json:"name,omitempty"`
	Alias string   `json:"alias,omitempty"`
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
	Alias string `json:"alias"`
	Name  string `json:"name,omitempty"`
	Error string `json:"error,omitempty"`
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

func ShellRequest(cols, rows uint16) ([]byte, error) {
	return json.Marshal(Request{Op: OpShell, TTY: true, Cols: cols, Rows: rows})
}

func SnapshotRequest(name string) ([]byte, error) {
	return json.Marshal(Request{Op: OpSnapshot, Name: name})
}

func ResumeRequest(alias string) ([]byte, error) {
	return json.Marshal(Request{Op: OpResume, Alias: alias})
}

func TerminateRequest() ([]byte, error) {
	return json.Marshal(Request{Op: OpTerminate})
}

func DecodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
