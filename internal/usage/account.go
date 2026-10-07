package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
)

// Diagnostic codes written to errors.log.
const (
	CodeInvalidStdin = "INVALID_STDIN"
	CodeUsageInvalid = "USAGE_INVALID"
	CodeAccountFile  = "ACCOUNT_FILE_UNREADABLE"
	CodeEmailInvalid = "EMAIL_INVALID"
	CodeNotSetup     = "NOT_SETUP"
	CodeNotShared    = "NOT_SHARED"
	CodeSpawnFailed  = "SPAWN_FAILED"
	CodeLogFailed    = "LOG_FAILED"
	CodeSendFailed   = "SEND_FAILED"
	CodeClockInvalid = "CLOCK_INVALID"

	// Codes for the session hooks and the detached claim-sync / send-usage
	// maintenance (claims, allowlist refresh, update check).
	CodeHookInput     = "HOOK_INPUT_INVALID"
	CodeClaimSync     = "CLAIM_SYNC_FAILED"
	CodeSharedRefresh = "SHARED_REFRESH_FAILED"
	CodeUpdateCheck   = "UPDATE_CHECK_FAILED"
)

// BackgroundFailedEvery throttles the maintenance codes above: they come from
// detached processes that run on every session start/end, so an outage must
// leave a breadcrumb, not a flood.
const BackgroundFailedEvery = 10 * time.Minute

// CodedError carries the errors.log code for a status line failure.
type CodedError struct {
	Code    string
	Message string
}

func (e *CodedError) Error() string { return e.Message }

// AccountEmail reads oauthAccount.emailAddress from path. Unlike
// claudecfg.ReadEmail it distinguishes unreadable from unparseable from
// invalid, because the status line logs a different code for each.
func AccountEmail(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", &CodedError{Code: CodeAccountFile, Message: "cannot read " + path}
	}
	email, err := emailField(raw)
	if err != nil {
		return "", &CodedError{Code: CodeAccountFile, Message: "cannot parse " + path}
	}
	if !claudecfg.ValidEmail(email) {
		return "", &CodedError{Code: CodeEmailInvalid, Message: "no valid oauthAccount.emailAddress in " + path}
	}
	return email, nil
}

// LoginEmail is claudecfg.ReadEmail for an account file path (as returned by
// claudecfg.AccountFileForEnv) instead of a config dir: lowercased, and a
// missing file or empty email is claudecfg.ErrNotLoggedIn. Errors name the
// path, never the address.
func LoginEmail(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", claudecfg.ErrNotLoggedIn
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	email, err := emailField(raw)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	if email == "" {
		return "", claudecfg.ErrNotLoggedIn
	}
	if !claudecfg.ValidEmail(email) {
		return "", fmt.Errorf("%s: oauthAccount.emailAddress is not a valid email", path)
	}
	return strings.ToLower(email), nil
}

// emailField extracts oauthAccount.emailAddress; a non-string value reads as "".
func emailField(raw []byte) (string, error) {
	var doc struct {
		OAuthAccount struct {
			EmailAddress any `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	email, _ := doc.OAuthAccount.EmailAddress.(string)
	return email, nil
}

// EnvConfigDir is the variable Claude Code sets for a non-default config dir.
// Resolve the dir with claudecfg.ActiveDir and, in hooks and the status line,
// the account file with claudecfg.AccountFileForEnv: unset reads
// ~/.claude.json, but an explicit CLAUDE_CONFIG_DIR=~/.claude reads
// ~/.claude/.claude.json, exactly like the Claude process that runs them.
const EnvConfigDir = "CLAUDE_CONFIG_DIR"
