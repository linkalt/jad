package sessionfs

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The profile_message policy of the workspace layout (docs/design_workspace_layout.md §30.3):
// one general deny rule list in three places, and the target workspace's allow_reset. Rules only
// refuse (deny) or allow one optional thing (a reset across workspaces); there is no rule order,
// and allow_reset cannot override a deny. The patterns are globs over the canonical addresses.

// The actions of a rule. A rule with message (the default) refuses the whole call, which implies
// the others; reset, compact and model_choice refuse only a call that asks for them.
const (
	MessageActionMessage     = "message"
	MessageActionReset       = "reset"
	MessageActionCompact     = "compact"
	MessageActionModelChoice = "model_choice"
)

var messageActions = []string{MessageActionMessage, MessageActionReset, MessageActionCompact, MessageActionModelChoice}

// MessageRule is one deny or allow_reset rule: the call's caller (From) and target (To) as
// canonical-address globs, and the actions the rule is about. An empty From or To is any.
type MessageRule struct {
	From, To string
	Actions  []string
}

// messagePatternBody is one segment of a pattern: address characters and the glob characters.
var messagePatternBody = regexp.MustCompile(`^[A-Za-z0-9._*?-]+$`)

// validMessagePattern reports whether pattern is a canonical address with globs: the local: or
// remote: prefix, then exactly four non-empty segments. '*' and '?' never cross a '/'.
func validMessagePattern(pattern string, localOnly bool) bool {
	body, found := strings.CutPrefix(pattern, LocalPrefix)
	if !found {
		if localOnly {
			return false
		}
		if body, found = strings.CutPrefix(pattern, RemotePrefix); !found {
			return false
		}
	}
	segments := strings.Split(body, "/")
	if len(segments) != 4 {
		return false
	}
	return !slices.ContainsFunc(segments, func(segment string) bool { return !messagePatternBody.MatchString(segment) })
}

func (r MessageRule) covers(action string) bool {
	if len(r.Actions) == 0 {
		return action == MessageActionMessage
	}
	return slices.Contains(r.Actions, action)
}

func globMatches(pattern, value string) bool {
	if pattern == "" {
		return true
	}
	ok, err := path.Match(pattern, value)
	return err == nil && ok
}

// matches reports whether the rule refuses a call from from to to asking for action. A call
// from a caller that has no local address (from empty, a remote caller) is matched only by a
// rule without a From: every From pattern starts with local:, which an empty address never has.
func (r MessageRule) matches(from, to, action string) bool {
	if !r.covers(action) {
		return false
	}
	if r.From != "" && !globMatches(r.From, from) {
		return false
	}
	return globMatches(r.To, to)
}

func matchesAny(rules []MessageRule, from, to, action string) bool {
	return slices.ContainsFunc(rules, func(rule MessageRule) bool { return rule.matches(from, to, action) })
}

// messageRuleKeys are the keys of a rule; allow_reset rules have no actions.
var messageRuleKeys = []string{"from", "to", "actions"}

// parseMessageRules reads a deny or allow_reset list. where names the file and key for errors.
// fromAllowed is false in a caller toolset, whose rules are always about what the toolset's own
// profiles send; actionsAllowed is false for allow_reset, which is about the reset action only;
// ownWorkspace, when set, is the only workspace an allow_reset target may name. localFrom keeps
// `from` to local addresses (a rule never names a remote caller).
func parseMessageRules(raw json.RawMessage, where string, fromAllowed, actionsAllowed bool, ownWorkspace string) ([]MessageRule, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, configError("%s must be an array of rules {from, to, actions}", where)
	}
	rules := make([]MessageRule, 0, len(entries))
	for index, entry := range entries {
		at := fmt.Sprintf("%s[%d]", where, index)
		if entry == nil {
			return nil, configError("%s must be an object {from, to, actions}", at)
		}
		var rule MessageRule
		for key, value := range entry {
			if !slices.Contains(messageRuleKeys, key) || key == "from" && !fromAllowed || key == "actions" && !actionsAllowed {
				return nil, configError("%s: %s is not a key of this rule%s", at, key, ruleKeyHint(key, fromAllowed, actionsAllowed))
			}
			switch key {
			case "from", "to":
				var pattern string
				if err := json.Unmarshal(value, &pattern); err != nil || !validMessagePattern(pattern, key == "from" || ownWorkspace != "") {
					return nil, configError("%s: %s must be a canonical address pattern: %s<workspace>/<session>/<category>/<name> with '*' and '?' (letters, digits, '.', '_', '-')%s", at, key, LocalPrefix, remoteForm(key, ownWorkspace))
				}
				if key == "from" {
					rule.From = pattern
				} else {
					rule.To = pattern
				}
			case "actions":
				var actions []string
				if err := json.Unmarshal(value, &actions); err != nil || len(actions) == 0 {
					return nil, configError("%s: actions must be a non-empty array of %s", at, strings.Join(messageActions, ", "))
				}
				for _, action := range actions {
					if !slices.Contains(messageActions, action) {
						return nil, configError("%s: action %q is unknown (%s)", at, action, strings.Join(messageActions, ", "))
					}
				}
				for i, action := range actions {
					if slices.Index(actions, action) != i {
						return nil, configError("%s: action %q is listed more than once", at, action)
					}
				}
				rule.Actions = actions
			}
		}
		if ownWorkspace != "" && rule.To != "" {
			workspace := strings.SplitN(strings.TrimPrefix(rule.To, LocalPrefix), "/", 2)[0]
			if workspace != ownWorkspace {
				return nil, configError("%s: to names workspace %q; an allow_reset of workspace %s allows resets of its own profiles only (local:%s/...)", at, workspace, ownWorkspace, ownWorkspace)
			}
		}
		for _, earlier := range rules {
			if earlier.From == rule.From && earlier.To == rule.To && slices.Equal(earlier.Actions, rule.Actions) {
				return nil, configError("%s repeats an earlier rule", at)
			}
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func ruleKeyHint(key string, fromAllowed, actionsAllowed bool) string {
	switch {
	case key == "from" && !fromAllowed:
		return "; a toolset's rules are about the profiles that use it, so they have no from (put a rule with from in workspace.json or global.json)"
	case key == "actions" && !actionsAllowed:
		return "; an allow_reset rule is about the reset action only"
	}
	return " (from, to" + map[bool]string{true: ", actions", false: ""}[actionsAllowed] + ")"
}

func remoteForm(key, ownWorkspace string) string {
	if key == "to" && ownWorkspace == "" {
		return " or " + RemotePrefix + "<binding>/<session>/<category>/<name>"
	}
	return ""
}

// MessageRefusal is the rule that refuses a profile_message call: who owns it (BySource: caller,
// global or workspace:<id>) and the action that was refused. Neither names a path or a rule.
type MessageRefusal struct {
	By     string
	Action string
}

// MessagePolicy is what refuses a call besides the caller's own toolset: the global rules and
// the rules of each workspace the call touches.
type MessagePolicy struct {
	Global []MessageRule
	// Caller and Target are the deny rules of the workspaces of the caller and of the target,
	// with their ids; Target is empty when the call stays in one workspace.
	CallerWorkspace, TargetWorkspace string
	Caller, Target                   []MessageRule
	// AllowReset are the target workspace's allow_reset rules.
	AllowReset []MessageRule
}

// Check judges one call between local addresses. actions are the actions the call asks for,
// message first. The first refusal is returned: global, caller workspace, target workspace, then
// allow_reset for a reset across workspaces. The outcome does not depend on the order.
func (p MessagePolicy) Check(from, to string, actions []string) *MessageRefusal {
	layers := []struct {
		by    string
		rules []MessageRule
	}{{"global", p.Global}, {"workspace:" + p.CallerWorkspace, p.Caller}}
	if p.TargetWorkspace != "" && p.TargetWorkspace != p.CallerWorkspace {
		layers = append(layers, struct {
			by    string
			rules []MessageRule
		}{"workspace:" + p.TargetWorkspace, p.Target})
	}
	for _, layer := range layers {
		for _, action := range actions {
			if matchesAny(layer.rules, from, to, action) {
				return &MessageRefusal{By: layer.by, Action: action}
			}
		}
	}
	if p.TargetWorkspace != "" && p.TargetWorkspace != p.CallerWorkspace && slices.Contains(actions, MessageActionReset) {
		if !slices.ContainsFunc(p.AllowReset, func(rule MessageRule) bool { return rule.matches(from, to, MessageActionMessage) }) {
			return &MessageRefusal{By: "workspace:" + p.TargetWorkspace, Action: MessageActionReset}
		}
	}
	return nil
}

// PolicyOf is the policy of a call from the workspace layout caller to the target workspace
// (both layouts, ids as served). A layout that is not a workspace has no rules.
func PolicyOf(caller, target *Layout, callerID, targetID string) MessagePolicy {
	policy := MessagePolicy{CallerWorkspace: callerID, TargetWorkspace: targetID}
	if caller != nil {
		policy.Global, policy.Caller = caller.GlobalMessageDeny, caller.MessageDeny
	}
	if target != nil {
		policy.Target, policy.AllowReset = target.MessageDeny, target.MessageAllowReset
		if caller == nil {
			policy.Global = target.GlobalMessageDeny
		}
	}
	return policy
}

// DeniesMessage reports whether a call from from to to is refused as a whole by the caller's
// own toolset rules, the global rules or the caller workspace's rules. The tool uses it to leave
// the profiles of its own session that it could never message out of its description.
func (s NativeProfileMessageSettings) DeniesMessage(from, to string) bool {
	return matchesAny(s.Deny, "", to, MessageActionMessage) || matchesAny(s.GlobalDeny, from, to, MessageActionMessage) || matchesAny(s.WorkspaceDeny, from, to, MessageActionMessage)
}

// DeniedBy is the refusal of the caller's own toolset rules for a call that asks for actions
// (message first), or nil. The toolset's rules have no from: they are about what its profiles send.
func (s NativeProfileMessageSettings) DeniedBy(to string, actions []string) *MessageRefusal {
	for _, action := range actions {
		if matchesAny(s.Deny, "", to, action) {
			return &MessageRefusal{By: "caller", Action: action}
		}
	}
	return nil
}

// ShownTargets are the profiles of the caller's own session, other than the caller, that the
// tool description lists: those no rule refuses as a whole. A refused profile is still
// addressable (the call is answered profile_denied), it is only not advertised.
func (s NativeProfileMessageSettings) ShownTargets(targets []string) []string {
	if s.Caller.Workspace == "" {
		return targets
	}
	from := MessageRef{Workspace: s.Caller.Workspace, Session: s.Caller.Session, Profile: s.Caller.Profile}.Canonical()
	shown := make([]string, 0, len(targets))
	for _, id := range targets {
		if !s.DeniesMessage(from, MessageRef{Workspace: s.Caller.Workspace, Session: s.Caller.Session, Profile: id}.Canonical()) {
			shown = append(shown, id)
		}
	}
	return shown
}
