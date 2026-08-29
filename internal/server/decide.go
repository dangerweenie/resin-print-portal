package server

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/dangerweenie/resin-print-portal/internal/store"
)

// Deny reason codes, surfaced to the Pi upload page and written to decision_log.
const (
	ReasonUnknownName        = "unknown_slack_name"
	ReasonAmbiguousName      = "ambiguous_name"
	ReasonMembershipInactive = "membership_inactive"
	ReasonNotCertified       = "not_certified"
	ReasonExtensionBlocked   = "extension_not_allowed"
	ReasonChecklist          = "checklist_incomplete"
	ReasonPendingApproval    = "printer_pending_approval"
)

// Outcome codes for decision_log.outcome.
const (
	OutcomeApproved       = "approved"
	OutcomeApprovedByName = "approved_by_name_match"
	OutcomeDenied         = "denied"
)

var wsRe = regexp.MustCompile(`\s+`)

// NormalizeName lowercases, trims, strips a leading '@', and collapses internal
// whitespace. It must stay in lockstep with the SQL used by
// store.ResolveSlackName's fallback branch.
func NormalizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	s = wsRe.ReplaceAllString(s, " ")
	return strings.ToLower(strings.TrimSpace(s))
}

// Decision is the result of checking whether a Slack name may print here.
type Decision struct {
	Allowed bool
	Outcome string
	Reason  string
	Member  *store.Member
}

func denied(reason string, m *store.Member) Decision {
	return Decision{Allowed: false, Outcome: OutcomeDenied, Reason: reason, Member: m}
}

// decideIdentity resolves a Slack name to a member and checks membership +
// certification. It does NOT check file extension or checklist — those are
// request-specific and handled by the print-request handler.
func (s *Server) decideIdentity(ctx context.Context, printerID int64, slackNameRaw string) (Decision, error) {
	norm := NormalizeName(slackNameRaw)
	if norm == "" {
		return denied(ReasonUnknownName, nil), nil
	}

	member, byName, err := s.st.ResolveSlackName(ctx, norm)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return denied(ReasonUnknownName, nil), nil
	case errors.Is(err, store.ErrAmbiguousName):
		return denied(ReasonAmbiguousName, nil), nil
	case err != nil:
		return Decision{}, err
	}

	if !member.Active {
		return denied(ReasonMembershipInactive, &member), nil
	}

	certified, err := s.st.IsCertified(ctx, member.ID, printerID)
	if err != nil {
		return Decision{}, err
	}
	if !certified {
		return denied(ReasonNotCertified, &member), nil
	}

	outcome := OutcomeApproved
	if byName {
		outcome = OutcomeApprovedByName
	}
	return Decision{Allowed: true, Outcome: outcome, Member: &member}, nil
}

// extensionAllowed reports whether filename's extension passes the printer's
// allow-list. An empty list allows anything.
func extensionAllowed(allowed []string, filename string) bool {
	if len(allowed) == 0 {
		return true
	}
	ext := strings.ToLower(filename)
	if i := strings.LastIndex(ext, "."); i >= 0 {
		ext = ext[i:]
	} else {
		ext = ""
	}
	for _, a := range allowed {
		if strings.ToLower(strings.TrimSpace(a)) == ext {
			return true
		}
	}
	return false
}
