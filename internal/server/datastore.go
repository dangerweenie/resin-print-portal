package server

import (
	"context"

	"github.com/dangerweenie/resin-print-portal/internal/store"
)

// DataStore is the slice of the persistence layer the HTTP handlers use.
// *store.Store satisfies it; tests provide fakes.
type DataStore interface {
	Ping(ctx context.Context) error

	// identity + certification
	ResolveSlackName(ctx context.Context, normalized string) (store.Member, bool, error)
	IsCertified(ctx context.Context, memberID, printerID int64) (bool, error)

	// printers
	GetPrinterByKeyHash(ctx context.Context, keyHash string) (store.Printer, error)
	GetPrinterBySlug(ctx context.Context, slug string) (store.Printer, error)
	ListPrinters(ctx context.Context) ([]store.Printer, error)
	CreatePrinter(ctx context.Context, p store.Printer) (store.Printer, error)
	EnrollPrinter(ctx context.Context, deviceID, hostname string, defaultChecklist []string, newKey, newKeyHash string) (store.Printer, bool, error)
	UpdatePrinter(ctx context.Context, p store.Printer) error
	RotatePrinterKey(ctx context.Context, id int64, apiKeyHash string) error
	SetPrinterApproved(ctx context.Context, id int64, approved bool) error
	DeletePrinter(ctx context.Context, id int64) error
	TouchPrinterSeen(ctx context.Context, id int64) error
	CountPendingPrinters(ctx context.Context) (int, error)

	// members + slack identities
	ListMembers(ctx context.Context) ([]store.Member, error)
	SlackIdentitiesFor(ctx context.Context, memberID int64) ([]store.SlackIdentity, error)
	AddSlackIdentity(ctx context.Context, memberID int64, normalized, addedBy string) error
	RemoveSlackIdentity(ctx context.Context, id int64) error

	// certifications
	Certify(ctx context.Context, memberID, printerID int64, by string) error
	Revoke(ctx context.Context, memberID, printerID int64) error
	ListCertifications(ctx context.Context, printerID int64) ([]store.CertifiedMember, error)

	// jobs
	CurrentJob(ctx context.Context, printerID int64) (store.PrintJob, error)
	CurrentJobs(ctx context.Context) ([]store.JobView, error)
	StartJob(ctx context.Context, j store.PrintJob) (store.PrintJob, error)
	EndJob(ctx context.Context, jobID int64, reason string) (bool, error)
	GetJob(ctx context.Context, printerID, jobID int64) (store.PrintJob, error)
	RecentJobs(ctx context.Context, printerID int64, limit int) ([]store.JobView, error)
	AllRecentJobs(ctx context.Context, limit int) ([]store.JobView, error)

	// decision log
	LogDecision(ctx context.Context, e store.DecisionLogEntry) error
	RecentDecisions(ctx context.Context, limit int) ([]store.DecisionLogView, error)

	// admins
	GetAdmin(ctx context.Context, username string) (store.Admin, error)
	SetAdminPassword(ctx context.Context, username, passwordHash string) error
}
