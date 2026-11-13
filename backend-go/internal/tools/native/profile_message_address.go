package native

import (
	"errors"
	"fmt"
	"slices"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// localAddress is a local profile address of the workspace layout (v2) after reading: the
// canonical reference, and what the messenger's request carries.
type localAddress struct {
	ref sessionfs.MessageRef
	// session is the request's Session: empty for a profile of the calling session, the bare
	// session id for another session of the calling workspace, <workspace>:<session> for a
	// session of another workspace.
	session string
	// target is <category>/<profile>.
	target string
}

// label is how results, handles and errors name the profile: the canonical address.
func (a localAddress) label() string { return a.ref.Canonical() }

var errUnknownLocalProfile = errors.New("profile is not available")

// parseLocalV2 reads a local address of the workspace layout written by the calling session.
// A syntax error is sessionfs.ErrInvalidMessageRef; a profile of the calling session that the
// session does not hold is errUnknownLocalProfile. Whether any other session, workspace or
// profile exists is the messenger's to say.
func (p *profileMessageTool) parseLocalV2(profile string) (localAddress, error) {
	caller := p.settings.Caller
	ref, err := sessionfs.ParseMessageRef(profile, caller)
	if err != nil {
		return localAddress{}, err
	}
	address := localAddress{ref: ref, target: ref.Profile}
	if ref.Own(caller) {
		if !slices.Contains(p.targets, ref.Profile) {
			return localAddress{}, errUnknownLocalProfile
		}
		return address, nil
	}
	address.session = ref.Reference(caller)
	return address, nil
}

// invalidProfileRef is the answer for an address that is none of the valid forms.
func (p *profileMessageTool) invalidProfileRef(callID, profile string) string {
	return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_profile_ref",
		fmt.Sprintf("%q is not a valid profile address; write %s", profile, sessionfs.MessageRefForms))
}
