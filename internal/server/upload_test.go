package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dangerweenie/resin-print-portal/internal/store"
)

func uploadMultipart(t *testing.T, fields map[string]string, fileName, fileBody string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if fileName != "" {
		fw, err := mw.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte(fileBody))
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestUploadStagesForCertifiedMember(t *testing.T) {
	fs := &fakeStore{
		printer:   store.Printer{ID: 7, Slug: "resin", DisplayName: "Resin", Approved: true},
		resolve:   func(string) (store.Member, bool, error) { return member(1, true), false, nil },
		certified: true,
	}
	s := newTestServer(t, fs)

	body, ct := uploadMultipart(t, map[string]string{
		"slack_name": "jane doe", "printer": "resin",
	}, "part.goo", "GOO-BYTES")
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fs.staged == nil {
		t.Fatal("StageJob was not called")
	}
	if fs.staged.Filename != "part.goo" || string(fs.stagedBytes) != "GOO-BYTES" {
		t.Errorf("staged job = %+v bytes=%q", fs.staged, fs.stagedBytes)
	}
	if fs.staged.MemberID == nil || *fs.staged.MemberID != 1 {
		t.Errorf("staged job member = %v, want 1", fs.staged.MemberID)
	}
}

func TestUploadRejectsUncertifiedMember(t *testing.T) {
	fs := &fakeStore{
		printer:   store.Printer{ID: 7, Slug: "resin", Approved: true},
		resolve:   func(string) (store.Member, bool, error) { return member(1, true), false, nil },
		certified: false,
	}
	s := newTestServer(t, fs)

	body, ct := uploadMultipart(t, map[string]string{"slack_name": "jane", "printer": "resin"}, "part.goo", "X")
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered form)", rec.Code)
	}
	if fs.staged != nil {
		t.Error("nothing should be staged for an uncertified member")
	}
	if !strings.Contains(rec.Body.String(), "not certified") {
		t.Errorf("expected a not-certified message: %s", rec.Body.String())
	}
}

func TestClaimReturnsStagedJobForTappedMember(t *testing.T) {
	fs := &fakeStore{
		printer:    store.Printer{ID: 7, Slug: "resin", Approved: true},
		resolveFob: func(string) (store.Member, error) { return member(1, true), nil },
		certified:  true,
		claimJob:   store.PrintJob{ID: 42, PrinterID: 7, Status: "printing"},
		claimMeta:  store.StagedMeta{JobID: 42, Filename: "part.goo", SHA256: "abc", SizeBytes: 9},
	}
	s := newTestServer(t, fs)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/printers/resin/claim",
		strings.NewReader(`{"rfid_code":"CAFE"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["claimed"] != true || resp["filename"] != "part.goo" || resp["sha256"] != "abc" {
		t.Fatalf("claim resp = %+v", resp)
	}
}

func TestClaimWhenNoStagedJob(t *testing.T) {
	fs := &fakeStore{
		printer:    store.Printer{ID: 7, Slug: "resin", Approved: true},
		resolveFob: func(string) (store.Member, error) { return member(1, true), nil },
		certified:  true,
		claimErr:   store.ErrNotFound,
	}
	s := newTestServer(t, fs)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/printers/resin/claim",
		strings.NewReader(`{"rfid_code":"CAFE"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["claimed"] != false || resp["reason"] != ReasonNoStagedJob {
		t.Fatalf("claim resp = %+v, want claimed:false no_staged_job", resp)
	}
}

func TestCheckConsumesCertCapture(t *testing.T) {
	fs := &fakeStore{
		printer:      store.Printer{ID: 7, Slug: "resin", Approved: true},
		resolveFob:   func(string) (store.Member, error) { return member(3, true), nil },
		certified:    false, // not yet — the tap is what certifies
		captureArmed: true,
		captureBy:    "captain",
	}
	s := newTestServer(t, fs)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/printers/resin/check",
		strings.NewReader(`{"rfid_code":"CAFE"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["certified"] != true || resp["reason"] != ReasonJustCertified {
		t.Fatalf("check resp = %+v, want certified:true certification_recorded", resp)
	}
	if len(fs.certifyCalls) != 1 || fs.certifyCalls[0][0] != 3 || fs.certifyCalls[0][1] != 7 {
		t.Fatalf("Certify calls = %v, want one for member 3 / printer 7", fs.certifyCalls)
	}
	if len(fs.logged) != 1 || fs.logged[0].Outcome != OutcomeCaptured {
		t.Fatalf("decision_log = %+v, want one captured_certification entry", fs.logged)
	}
}
