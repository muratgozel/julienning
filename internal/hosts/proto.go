package hosts

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sessions"
)

// Proto is the agent protocol version (SPEC "Agent"). Within one proto a
// verb only gains optional fields; hello, inventory and the run envelope
// are frozen across protos (fields are only ever added), so host add, host
// list, update --host and uninstall --host work between any two versions
// that have the agent.
//
// CRITICAL release rule: a change to the agent protocol or the transfer
// archive format bumps julienning's minor version (cross-machine commands
// need the same major.minor on both ends); Proto is the second check, in
// case a release gets the bump wrong.
const Proto = 1

// FramePrefix starts the line that precedes every agent answer except run
// and launch. Anything before it is discarded: a startup file that prints
// for non-interactive shells would otherwise corrupt the JSON.
const FramePrefix = "JULIENNING-AGENT/"

// Frame is this julienning's frame line, without the newline.
func Frame() string { return FramePrefix + strconv.Itoa(Proto) }

// Agent error codes, carried in {"error": {"code", "message"}} after the
// frame line with exit status 1.
const (
	CodeNotSetUp          = "NOT_SET_UP"
	CodeMachineMismatch   = "MACHINE_MISMATCH"
	CodeWorkerMismatch    = "WORKER_MISMATCH"
	CodeWorkerUnreachable = "WORKER_UNREACHABLE"
	CodeProto             = "PROTO"
	CodeNotLocal          = "NOT_LOCAL"
	CodePersonal          = "PERSONAL"
	CodeNoCheckout        = "NO_CHECKOUT"
	CodeAmbiguous         = "AMBIGUOUS"
	CodePathMissing       = "PATH_MISSING"
	CodeLive              = "LIVE"
	CodeMaybeOpen         = "MAYBE_OPEN"
	CodeCollision         = "COLLISION"
	CodeGone              = "GONE"
	CodeChanged           = "CHANGED"
	CodeNoSpace           = "NO_SPACE"
	CodeInvalid           = "INVALID"
)

// AgentError is an error answer, or one the agent returns before writing it.
type AgentError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *AgentError) Error() string { return e.Message }

// Errorf builds an AgentError.
func Errorf(code, format string, a ...any) *AgentError {
	return &AgentError{Code: code, Message: fmt.Sprintf(format, a...)}
}

type errorBody struct {
	Error *AgentError `json:"error"`
}

// ToAgentError gives any error an agent code: an *AgentError as it is, a
// move refusal its kind (LIVE, COLLISION, …), anything else INVALID, the
// message kept either way.
func ToAgentError(err error) *AgentError {
	var ae *AgentError
	if errors.As(err, &ae) {
		return ae
	}
	if kind := sessions.ErrorKind(err); kind != "" {
		return Errorf(kind, "%s", err.Error())
	}
	return Errorf(CodeInvalid, "%s", err.Error())
}

// EncodeAnswer writes the frame line and v as JSON (an error as the error
// body, through ToAgentError: an error value has no fields to encode).
func EncodeAnswer(v any) []byte {
	var b bytes.Buffer
	b.WriteString(Frame() + "\n")
	if err, ok := v.(error); ok {
		v = errorBody{Error: ToAgentError(err)}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		raw, _ = json.Marshal(errorBody{Error: Errorf(CodeInvalid, "encode the answer: %v", err)})
	}
	b.Write(raw)
	b.WriteByte('\n')
	return b.Bytes()
}

// ErrNoFrame means the output had no frame line: no julienning agent
// answered.
var ErrNoFrame = errors.New("no frame line")

// DecodeAnswer finds the frame line in out (startup-file noise before it is
// dropped) and decodes the JSON after it into v. An error body becomes an
// *AgentError. proto is the answering agent's protocol, 0 when no frame was
// found. It does not refuse another proto: hello is frozen across protos
// and is what tells the controller about the mismatch (Hello.Proto); every
// other call comes after that check.
func DecodeAnswer(out []byte, v any) (proto int, err error) {
	rest, proto, ok := afterFrame(out)
	if !ok {
		return 0, ErrNoFrame
	}
	var eb errorBody
	if json.Unmarshal(rest, &eb) == nil && eb.Error != nil {
		return proto, eb.Error
	}
	if err := json.Unmarshal(rest, v); err != nil {
		return proto, fmt.Errorf("unreadable answer: %v", err)
	}
	return proto, nil
}

// afterFrame returns what follows the first frame line at the start of a
// line, and the proto it names.
func afterFrame(out []byte) ([]byte, int, bool) {
	for i := 0; i < len(out); {
		end := bytes.IndexByte(out[i:], '\n')
		line := out[i:]
		next := len(out)
		if end >= 0 {
			line, next = out[i:i+end], i+end+1
		}
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte(FramePrefix)) {
			if n, err := strconv.Atoi(string(line[len(FramePrefix):])); err == nil && n > 0 {
				return out[next:], n, true
			}
		}
		i = next
	}
	return nil, 0, false
}

// HelloPaths are a host's absolute paths as its agent finds them.
type HelloPaths struct {
	Julienning string  `json:"julienning"`
	Claude     *string `json:"claude"`
	Tmux       *string `json:"tmux"`
}

// TmuxMode is a host's tmux mode; nil in Hello when never decided.
type TmuxMode struct {
	Enabled bool   `json:"enabled"`
	Session string `json:"session,omitempty"`
}

// Hello is the answer of `agent hello`, the first call of every
// cross-machine command. Frozen across protos: fields are only added.
type Hello struct {
	Proto         int        `json:"proto"`
	Version       string     `json:"version"`
	OS            string     `json:"os"`
	Arch          string     `json:"arch"`
	Home          string     `json:"home"`
	MachineID     *string    `json:"machine_id"`
	Dev           *string    `json:"dev"`
	WorkerURL     *string    `json:"worker_url"`
	SetUp         bool       `json:"set_up"`
	Managed       bool       `json:"managed"`
	SetupWarning  string     `json:"setup_warning"`
	Tmux          *TmuxMode  `json:"tmux"`
	Paths         HelloPaths `json:"paths"`
	ClaudeVersion *string    `json:"claude_version"`
	TmuxVersion   *string    `json:"tmux_version"`
}

// ID is the machine id, "" before setup.
func (h *Hello) ID() string {
	if h.MachineID == nil {
		return ""
	}
	return *h.MachineID
}

// BootstrapRequest is `agent bootstrap`'s stdin: this machine's team
// settings. The token travels only on stdin (other users of a shared host
// see command lines in ps).
type BootstrapRequest struct {
	Dev           string `json:"dev"`
	WorkerURL     string `json:"worker_url"`
	Token         string `json:"token"`
	ReplaceWorker bool   `json:"replace_worker"`
	// ReplaceToken stores the token over a set-up host's own token for the
	// same Worker; sent after TokenRejected and the user's yes.
	ReplaceToken bool `json:"replace_token,omitempty"`
}

// BootstrapResult is `agent bootstrap`'s answer.
type BootstrapResult struct {
	Created        bool `json:"created"`
	TokenUpdated   bool `json:"token_updated"`
	WorkerReplaced bool `json:"worker_replaced"`
	// TokenRejected: set up for the same Worker, whose token the Worker
	// rejects (rotated); nothing changed.
	TokenRejected bool `json:"token_rejected,omitempty"`
	// WorkerError: the token could not be checked from the host.
	WorkerError string `json:"worker_error,omitempty"`
}

// InventoryConfig is one registered dir in `agent inventory`.
type InventoryConfig struct {
	Name     string `json:"name"`
	Dir      string `json:"dir"`
	Email    string `json:"email"`    // "" when not logged in or unreadable
	Nickname string `json:"nickname"` // "" when none or not shared
	Shared   bool   `json:"shared"`
	Current  bool   `json:"current"`
}

// Inventory is `agent inventory`'s answer. Frozen across protos.
type Inventory struct {
	Configs []InventoryConfig `json:"configs"`
}

// RunRequest is `agent run`'s envelope, one base64url argument (stdin is
// the terminal). Frozen across protos: fields are only added. Project is
// set only for commands that depend on the working directory; the agent
// runs them in its checkout of it (pin and unpin fail without one), every
// other command in the host's home.
type RunRequest struct {
	MachineID string   `json:"machine_id"`
	Argv      []string `json:"argv"`
	Project   *Project `json:"project"`
	TZ        string   `json:"tz,omitempty"`
	NoColor   bool     `json:"no_color,omitempty"`
	// Host is the name the controller knows the host by, for messages the
	// agent prints itself (`julienning: box: …`, "no checkout on box").
	Host string `json:"host,omitempty"`
}

// InstallSelfResult is `agent install-self`'s answer.
type InstallSelfResult struct {
	Version string `json:"version"`
	Link    string `json:"link"`
}

// EncodeArg encodes v as one unpadded base64url argument (RFC 4648 §5): only
// [A-Za-z0-9_-], inert in any shell, fish included.
func EncodeArg(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeArg reverses EncodeArg.
func DecodeArg(s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Errorf(CodeInvalid, "the request is not base64url: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return Errorf(CodeInvalid, "the request is not valid JSON: %v", err)
	}
	return nil
}

// LocateRequest is `agent locate`'s stdin: find the project's checkout on
// this machine (SPEC "Locating a project"), or check Path, a checkout the
// user typed.
type LocateRequest struct {
	Project *Project `json:"project"`
	// Branch is preferred among several checkouts (the session's).
	Branch string `json:"branch,omitempty"`
	// Machine is the controller's name for this machine; RanIn says where
	// the session ran, for messages.
	Machine string `json:"machine"`
	RanIn   string `json:"ran_in,omitempty"`
	Path    string `json:"path,omitempty"`
}

// SessionsRequest is `agent sessions`'s stdin: the sessions transfer may
// move from this machine, in its checkout of Project (nil: every project).
type SessionsRequest struct {
	Project *Project `json:"project"`
	All     bool     `json:"all"`
	Limit   int      `json:"limit"`
	Machine string   `json:"machine"`
}

// SessionsAnswer lists them: Cwd is the directory listed ("" with all),
// Labels name each config dir as picker rows do, Projects is each listed
// session's project, computed here (the destination locates it).
type SessionsAnswer struct {
	Cwd      string              `json:"cwd"`
	Sessions []sessions.Session  `json:"sessions"`
	Labels   map[string]string   `json:"labels"`
	Projects map[string]*Project `json:"projects"`
	Warnings []string            `json:"warnings,omitempty"`
}

// ChooseRequest is `agent choose`'s stdin: TARGET, or "" to rank like next;
// TZ renders the warnings' times in the controller's zone.
type ChooseRequest struct {
	Target string `json:"target"`
	TZ     string `json:"tz,omitempty"`
}

// ChooseAnswer is the account the session moves into; it is now this
// machine's selection.
type ChooseAnswer struct {
	Config   string          `json:"config"`
	Dir      string          `json:"dir"`
	Email    string          `json:"email"`
	Nickname string          `json:"nickname"`
	Account  *remote.Account `json:"account"`
	Warnings []string        `json:"warnings,omitempty"`
}

// ExportRequest is `agent export`'s stdin; the answer is the frame line,
// then the archive's header line and the archive (sessions.Pack).
type ExportRequest struct {
	ID        string `json:"id"`
	ConfigDir string `json:"config_dir"`
}

// ReceiveRequest is the first line of `agent receive`'s stdin, written by
// the controller ahead of the export's header line and archive: where the
// session goes (choose's account, locate's directory).
type ReceiveRequest struct {
	ConfigDir string `json:"config_dir"`
	Email     string `json:"email"`
	Cwd       string `json:"cwd"`
	Checkout  string `json:"checkout"`
	GitRoot   string `json:"git_root"`
	From      string `json:"from"`
}

// ReleaseRequest is `agent release`'s stdin.
type ReleaseRequest struct {
	ID             string `json:"id"`
	ConfigDir      string `json:"config_dir"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

// ReleaseAnswer is `agent release`'s answer.
type ReleaseAnswer struct {
	Released bool     `json:"released"`
	Warnings []string `json:"warnings,omitempty"`
}

// LaunchRequest is `agent launch`'s argument: resume a session (Args) in
// Dir's account from Cwd, in this machine's tmux mode.
type LaunchRequest struct {
	MachineID string   `json:"machine_id"`
	Dir       string   `json:"dir"`
	Cwd       string   `json:"cwd"`
	Args      []string `json:"args"`
	Host      string   `json:"host,omitempty"`
}
