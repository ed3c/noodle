package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const (
	testBaseSHA       = "1111111111111111111111111111111111111111"
	testSender        = "ed3c-noodles-protection-audit[bot]"
	testCommentAuthor = "github-actions[bot]"
)

type fakeGitHub struct {
	mu               sync.Mutex
	issues           map[int]Issue
	comments         map[int][]Comment
	nextCommentID    int64
	commentPosts     int
	issueEdits       int
	dispatchPosts    int
	modelInvocations int
	unreadable       map[int]bool
	issueReads       map[int]int
	mutateOnRead     map[int]func(*Issue)
	baseSHA          string
	branches         map[string]string
	branchReads      map[string]int
	pulls            map[int]PullRequest
	nextPullNumber   int
	pullPosts        int
	pullFiles        map[int][]PullRequestFile
	mergePosts       int
	closePosts       int
	mergeHead        string
	wrongMergeHead   bool
	closeBeforeMerge bool
}

func newFakeGitHub(t *testing.T, issues ...Issue) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		issues:        make(map[int]Issue),
		comments:      make(map[int][]Comment),
		nextCommentID: 1,
		unreadable:    make(map[int]bool),
		issueReads:    make(map[int]int),
		mutateOnRead:  make(map[int]func(*Issue)),
		baseSHA:       testBaseSHA, branches: make(map[string]string), branchReads: make(map[string]int),
		pulls: make(map[int]PullRequest), pullFiles: make(map[int][]PullRequestFile), nextPullNumber: 1,
	}
	for _, issue := range issues {
		f.issues[issue.Number] = issue
	}
	return f
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func (f *fakeGitHub) client() *GitHubClient {
	client := NewGitHubClient("https://github.test", "test-token")
	client.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		f.serveHTTP(recorder, request)
		return recorder.Result(), nil
	})}
	return client
}

func (f *fakeGitHub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/repos/ed3c/noodle":
		writeJSON(w, Repository{FullName: targetRepository, DefaultBranch: defaultBranch})
	case r.Method == http.MethodGet && path == "/repos/ed3c/noodle/git/ref/heads/main":
		writeJSON(w, GitRef{Object: GitObject{SHA: f.baseSHA}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/ed3c/noodle/git/ref/heads/"):
		branch := strings.TrimPrefix(path, "/repos/ed3c/noodle/git/ref/heads/")
		f.branchReads[branch]++
		sha, ok := f.branches[branch]
		if !ok || f.branchReads[branch] == 1 {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, GitRef{Ref: "refs/heads/" + branch, Object: GitObject{SHA: sha}})
	case r.Method == http.MethodGet && path == "/repos/ed3c/noodle/pulls":
		out := make([]PullRequest, 0, len(f.pulls))
		for _, pull := range f.pulls {
			if pull.State == "open" {
				out = append(out, pull)
			}
		}
		writeJSON(w, out)
	case r.Method == http.MethodPost && path == "/repos/ed3c/noodle/pulls":
		var input struct{ Title, Head, Body, Base string }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		pull := PullRequest{Number: f.nextPullNumber, State: "open", Body: input.Body}
		pull.Head.Ref = input.Head
		pull.Head.SHA = f.branches[input.Head]
		pull.Base.Ref = input.Base
		f.nextPullNumber++
		f.pullPosts++
		f.pulls[pull.Number] = pull
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, pull)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/ed3c/noodle/pulls/"):
		if strings.HasSuffix(path, "/files") {
			var number int
			_, _ = fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/ed3c/noodle/pulls/"), "/files"), "%d", &number)
			writeJSON(w, f.pullFiles[number])
			return
		}
		var number int
		_, _ = fmt.Sscanf(strings.TrimPrefix(path, "/repos/ed3c/noodle/pulls/"), "%d", &number)
		pull, ok := f.pulls[number]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, pull)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/merge"):
		var number int
		_, _ = fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/ed3c/noodle/pulls/"), "/merge"), "%d", &number)
		var input struct {
			SHA string `json:"sha"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mergePosts++
		f.mergeHead = input.SHA
		pull := f.pulls[number]
		pull.Merged = true
		pull.State = "closed"
		pull.MergeCommitSHA = "2222222222222222222222222222222222222222"
		if f.wrongMergeHead {
			pull.Head.SHA = strings.Repeat("4", 40)
		}
		f.pulls[number] = pull
		f.baseSHA = pull.MergeCommitSHA
		writeJSON(w, MergeResult{Merged: true, SHA: pull.MergeCommitSHA})
	case r.Method == http.MethodGet && path == "/repos/ed3c/noodle/issues":
		out := make([]Issue, 0, len(f.issues))
		for _, issue := range f.issues {
			if issue.State == "open" {
				out = append(out, issue)
			}
		}
		writeJSON(w, out)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/ed3c/noodle/issues/") && strings.HasSuffix(path, "/comments"):
		number := issueNumberFromPath(path)
		if f.unreadable[number] {
			http.Error(w, `{"message":"comments unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, f.comments[number])
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/repos/ed3c/noodle/issues/") && strings.HasSuffix(path, "/comments"):
		var input struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		number := issueNumberFromPath(path)
		comment := trustedComment(f.nextCommentID, input.Body)
		f.nextCommentID++
		f.commentPosts++
		f.comments[number] = append(f.comments[number], comment)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, comment)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/ed3c/noodle/issues/"):
		number := issueNumberFromPath(path)
		issue, ok := f.issues[number]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.issueReads[number]++
		if mutate := f.mutateOnRead[f.issueReads[number]]; mutate != nil {
			mutate(&issue)
			f.issues[number] = issue
		}
		writeJSON(w, issue)
	case r.Method == http.MethodPatch && strings.Contains(path, "/issues/"):
		var input struct {
			Body  string `json:"body"`
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		number := issueNumberFromPath(path)
		issue := f.issues[number]
		if input.Body != "" {
			issue.Body = input.Body
			f.issueEdits++
		}
		if input.State == "closed" {
			if f.mergePosts == 0 {
				f.closeBeforeMerge = true
			}
			issue.State = "closed"
			issue.StateReason = "completed"
			f.closePosts++
		}
		f.issues[number] = issue
		writeJSON(w, issue)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/dispatches"):
		f.dispatchPosts++
		w.WriteHeader(http.StatusNoContent)
	case strings.Contains(path, "/models"):
		f.modelInvocations++
		writeJSON(w, map[string]any{})
	default:
		http.Error(w, fmt.Sprintf("unexpected %s %s", r.Method, path), http.StatusNotFound)
	}
}

func issueNumberFromPath(path string) int {
	var number int
	_, _ = fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/ed3c/noodle/issues/"), "/comments"), "%d", &number)
	return number
}

func writeJSON(w http.ResponseWriter, value any) {
	_ = json.NewEncoder(w).Encode(value)
}

func issueBody(number int) string {
	return fmt.Sprintf(`<!-- noodles-role: repository-mutating-atom -->
<!-- noodles-target: ed3c/noodle -->
<!-- noodles-subject: ed3c/noodle#%d -->
<!-- noodles-state: ready -->
<!-- noodles-component: contract -->
<!-- noodles-executor: local-noodle -->
<!-- noodles-runtime: host-toolchain -->
<!-- noodles-write-boundary: policy, adapters/noodles-github -->
<!-- noodles-evidence: github-only-v1 -->

## Goal

Exercise the target consumer.
`, number)
}

func bodySHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func testPayload(number int, body string) DispatchPayload {
	return DispatchPayload{
		SourceRepository:  sourceRepository,
		Target:            targetRepository,
		Subject:           fmt.Sprintf("%s#%d", targetRepository, number),
		SubjectBodySHA256: bodySHA(body),
		BaseSHA:           testBaseSHA,
		Runtime:           "host-toolchain",
		Evidence:          "github-only-v1",
		WriteBoundary:     []string{"policy", "adapters/noodles-github"},
	}
}

func testEvent(t *testing.T, payload DispatchPayload, sender string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"action":         dispatchEventType,
		"repository":     map[string]string{"full_name": targetRepository},
		"sender":         map[string]string{"login": sender},
		"client_payload": payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testPolicy() Policy {
	return Policy{
		SchemaVersion:              1,
		Repository:                 targetRepository,
		AllowedRepositories:        []string{targetRepository, sourceRepository},
		DefaultBranch:              defaultBranch,
		CrossRepositoryStatus:      crossRepositoryAdmitted,
		RepositoryDispatchSender:   testSender,
		AuthorizationCommentAuthor: testCommentAuthor,
	}
}

func TestNoodlesDispatchAdmission(t *testing.T) {
	body := issueBody(17)
	fake := newFakeGitHub(t, Issue{Number: 17, Title: "Target issue", State: "open", Body: body})
	client := fake.client()
	payload := testPayload(17, body)

	first, err := Receive(context.Background(), client, testPolicy(), testCapabilities(), testEvent(t, payload, testSender), testBaseSHA)
	if err != nil {
		t.Fatalf("receive first delivery: %v", err)
	}
	if first.Status != "authorized" || first.DispatchIdentity == "" {
		t.Fatalf("first result = %#v", first)
	}
	second, err := Receive(context.Background(), client, testPolicy(), testCapabilities(), testEvent(t, payload, testSender), testBaseSHA)
	if err != nil {
		t.Fatalf("receive retry: %v", err)
	}
	if second.Status != "reused" || second.DispatchIdentity != first.DispatchIdentity {
		t.Fatalf("retry result = %#v, first = %#v", second, first)
	}
	if fake.commentPosts != 1 {
		t.Fatalf("comment posts = %d, want 1", fake.commentPosts)
	}
	receipt, err := ParseAuthorization(fake.comments[17][0].Body)
	if err != nil {
		t.Fatalf("parse stored authorization: %v", err)
	}
	if !reflect.DeepEqual(receipt.Declaration, payload) || receipt.Sender != testSender || receipt.DispatchIdentity != first.DispatchIdentity {
		t.Fatalf("stored receipt = %#v", receipt)
	}
}

func TestNoodlesDispatchAdmissionDoesNotReuseForgedComment(t *testing.T) {
	body := issueBody(17)
	fake := newFakeGitHub(t, Issue{Number: 17, Title: "Target issue", State: "open", Body: body})
	payload := testPayload(17, body)
	forged := Authorization{SchemaVersion: 1, Sender: testSender, Declaration: payload}
	forged.DispatchIdentity, _ = DispatchIdentity(payload)
	fake.comments[17] = []Comment{{ID: 41, Body: FormatAuthorization(forged), User: User{Login: "outsider"}}}
	fake.nextCommentID = 42

	result, err := Receive(context.Background(), fake.client(), testPolicy(), testCapabilities(), testEvent(t, payload, testSender), testBaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "authorized" || result.CommentID != 42 || fake.commentPosts != 1 {
		t.Fatalf("result=%#v comment posts=%d", result, fake.commentPosts)
	}
	items, diagnostics, err := Sync(context.Background(), fake.client(), testPolicy(), testCapabilities())
	if err != nil || len(items) != 1 || len(diagnostics) != 0 {
		t.Fatalf("items=%#v diagnostics=%#v err=%v", items, diagnostics, err)
	}
}

func TestNoodlesDispatchAdmissionPlantedNegativesLeaveNoResidue(t *testing.T) {
	body := issueBody(17)
	base := testPayload(17, body)
	cases := []struct {
		name   string
		mutate func(*DispatchPayload, *Policy, *string)
	}{
		{"wrong target", func(p *DispatchPayload, _ *Policy, _ *string) { p.Target = "ed3c/other" }},
		{"foreign subject", func(p *DispatchPayload, _ *Policy, _ *string) { p.Subject = "ed3c/noodles#17" }},
		{"stale digest", func(p *DispatchPayload, _ *Policy, _ *string) { p.SubjectBodySHA256 = strings.Repeat("0", 64) }},
		{"stale base", func(p *DispatchPayload, _ *Policy, _ *string) { p.BaseSHA = strings.Repeat("2", 40) }},
		{"wrong sender", func(_ *DispatchPayload, _ *Policy, sender *string) { *sender = "wrong[bot]" }},
		{"held policy", func(_ *DispatchPayload, policy *Policy, _ *string) { policy.CrossRepositoryStatus = "HOLD" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGitHub(t, Issue{Number: 17, Title: "Target issue", State: "open", Body: body})
			payload := base
			payload.WriteBoundary = append([]string(nil), base.WriteBoundary...)
			policy := testPolicy()
			sender := testSender
			tc.mutate(&payload, &policy, &sender)
			_, err := Receive(context.Background(), fake.client(), policy, testCapabilities(), testEvent(t, payload, sender), testBaseSHA)
			if err == nil {
				t.Fatal("expected admission refusal")
			}
			if fake.commentPosts != 0 || fake.issueEdits != 0 || fake.dispatchPosts != 0 || fake.modelInvocations != 0 {
				t.Fatalf("residue: comments=%d edits=%d dispatches=%d models=%d", fake.commentPosts, fake.issueEdits, fake.dispatchPosts, fake.modelInvocations)
			}
		})
	}
}

func TestIssueContractRequiresExactSubjectMarker(t *testing.T) {
	body := strings.Replace(issueBody(17), "<!-- noodles-subject: ed3c/noodle#17 -->\n", "", 1)
	if _, err := parseIssueContract(body, 17); err == nil {
		t.Fatal("missing subject marker must be rejected")
	}
}

func TestNoodlesGitHubTargetConsumerRejectsForeignSubject(t *testing.T) {
	body := issueBody(17)
	fake := newFakeGitHub(t, Issue{Number: 17, Title: "Target issue", State: "open", Body: body})
	receipt := Authorization{
		SchemaVersion: 1,
		Sender:        testSender,
		Declaration:   testPayload(17, body),
	}
	receipt.Declaration.Subject = "ed3c/noodles#17"
	receipt.DispatchIdentity, _ = DispatchIdentity(receipt.Declaration)
	fake.comments[17] = []Comment{trustedComment(1, FormatAuthorization(receipt))}

	items, diagnostics, err := Sync(context.Background(), fake.client(), testPolicy(), testCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || len(diagnostics) != 1 || diagnostics[0].Code != "foreign_subject" {
		t.Fatalf("items=%#v diagnostics=%#v", items, diagnostics)
	}
}

func TestNoodlesGitHubTargetConsumer(t *testing.T) {
	body := issueBody(17)
	fake := newFakeGitHub(t,
		Issue{Number: 17, Title: "Authorized", State: "open", Body: body},
		Issue{Number: 18, Title: "Missing receipt", State: "open", Body: issueBody(18)},
	)
	payload := testPayload(17, body)
	if _, err := Receive(context.Background(), fake.client(), testPolicy(), testCapabilities(), testEvent(t, payload, testSender), testBaseSHA); err != nil {
		t.Fatalf("authorize target Issue: %v", err)
	}

	items, diagnostics, err := Sync(context.Background(), fake.client(), testPolicy(), testCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "ed3c/noodle#17" || items[0].ExecutionSkill != "noodles-issue-execute" {
		t.Fatalf("items = %#v", items)
	}
	if len(diagnostics) != 1 || diagnostics[0].Subject != "ed3c/noodle#18" || diagnostics[0].Code != "missing_receipt" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}

	restartedItems, restartedDiagnostics, err := Sync(context.Background(), fake.client(), testPolicy(), testCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restartedItems, items) || !reflect.DeepEqual(restartedDiagnostics, diagnostics) {
		t.Fatalf("restart drifted: items=%#v diagnostics=%#v", restartedItems, restartedDiagnostics)
	}
}

func TestNoodlesGitHubTargetConsumerRejectsIssueChangedDuringSweep(t *testing.T) {
	body := issueBody(17)
	fake := newFakeGitHub(t, Issue{Number: 17, Title: "Authorized", State: "open", Body: body})
	receipt := Authorization{SchemaVersion: 1, Sender: testSender, Declaration: testPayload(17, body)}
	receipt.DispatchIdentity, _ = DispatchIdentity(receipt.Declaration)
	fake.comments[17] = []Comment{trustedComment(1, FormatAuthorization(receipt))}
	fake.mutateOnRead[2] = func(issue *Issue) {
		issue.Body += "\nthird-party edit\n"
	}

	items, diagnostics, err := Sync(context.Background(), fake.client(), testPolicy(), testCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || len(diagnostics) != 1 || diagnostics[0].Code != "provider_changed" {
		t.Fatalf("items=%#v diagnostics=%#v", items, diagnostics)
	}
}

func TestNoodlesGitHubTargetConsumerFailClosedDiagnostics(t *testing.T) {
	body := issueBody(17)
	base := Authorization{SchemaVersion: 1, Sender: testSender, Declaration: testPayload(17, body)}
	base.DispatchIdentity, _ = DispatchIdentity(base.Declaration)
	tests := []struct {
		name     string
		comments []Comment
		prepare  func(*fakeGitHub)
		code     string
	}{
		{name: "missing", code: "missing_receipt"},
		{name: "stale digest", comments: []Comment{trustedComment(1, staleAuthorization(base, func(receipt *Authorization) {
			receipt.Declaration.SubjectBodySHA256 = strings.Repeat("0", 64)
		}))}, code: "stale_receipt"},
		{name: "stale base", comments: []Comment{trustedComment(1, staleAuthorization(base, func(receipt *Authorization) {
			receipt.Declaration.BaseSHA = strings.Repeat("2", 40)
		}))}, code: "stale_receipt"},
		{name: "wrong sender", comments: []Comment{trustedComment(1, staleAuthorization(base, func(receipt *Authorization) {
			receipt.Sender = "wrong[bot]"
		}))}, code: "stale_receipt"},
		{name: "malformed", comments: []Comment{trustedComment(1, authorizationMarker+`{}`)}, code: "malformed_receipt"},
		{name: "trailing receipt", comments: []Comment{trustedComment(1, FormatAuthorization(base)+` {}`)}, code: "malformed_receipt"},
		{name: "forged commenter", comments: []Comment{{ID: 1, Body: FormatAuthorization(base), User: User{Login: "outsider"}}}, code: "untrusted_receipt_author"},
		{name: "malformed outsider", comments: []Comment{{ID: 1, Body: authorizationMarker + `{}`, User: User{Login: "outsider"}}}, code: "untrusted_receipt_author"},
		{name: "duplicate", comments: []Comment{trustedComment(1, FormatAuthorization(base)), trustedComment(2, FormatAuthorization(base))}, code: "duplicate_receipt"},
		{name: "unreadable", prepare: func(fake *fakeGitHub) { fake.unreadable[17] = true }, code: "provider_unreadable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGitHub(t, Issue{Number: 17, Title: "Target issue", State: "open", Body: body})
			fake.comments[17] = tc.comments
			if tc.prepare != nil {
				tc.prepare(fake)
			}
			items, diagnostics, err := Sync(context.Background(), fake.client(), testPolicy(), testCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 0 || len(diagnostics) != 1 || diagnostics[0].Code != tc.code {
				t.Fatalf("items=%#v diagnostics=%#v", items, diagnostics)
			}
		})
	}
}

func staleAuthorization(original Authorization, mutate func(*Authorization)) string {
	receipt := original
	receipt.Declaration.WriteBoundary = append([]string(nil), original.Declaration.WriteBoundary...)
	mutate(&receipt)
	receipt.DispatchIdentity, _ = DispatchIdentity(receipt.Declaration)
	return FormatAuthorization(receipt)
}

func trustedComment(id int64, body string) Comment {
	return Comment{ID: id, Body: body, User: User{Login: testCommentAuthor}}
}

func TestDispatchIdentityMatchesSourceCanonicalJSON(t *testing.T) {
	payload := testPayload(17, issueBody(17))
	payload.SubjectBodySHA256 = strings.Repeat("x", 64)
	identity, err := DispatchIdentity(payload)
	if err != nil {
		t.Fatal(err)
	}
	if identity != "72755299ac23feef9c801a685ef64691870ae654857296fefe37e5cfb177b63b" {
		t.Fatalf("identity = %s", identity)
	}
}

func TestCapabilitiesFailClosedWhenRequiredCarrierIsMissing(t *testing.T) {
	capabilities := testCapabilities()
	delete(capabilities.VerificationSurfaces, "worktree-execution")
	if err := validateCapabilities(capabilities); err == nil {
		t.Fatal("missing worktree-execution carrier must be rejected")
	}
}

func TestStrictPolicyJSONRejectsTrailingData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "github.json")
	data, err := json.Marshal(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte(` {}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStrictJSON[Policy](path); err == nil {
		t.Fatal("trailing policy JSON must be rejected")
	}
}

func TestDoneIsProviderReadOnlyAndTargetScoped(t *testing.T) {
	dir := t.TempDir()
	policyPath, capabilitiesPath := writeFixtureFiles(t, dir)
	t.Setenv("NOODLES_GITHUB_POLICY", policyPath)
	t.Setenv("NOODLES_REPO_CAPABILITIES", capabilitiesPath)
	if err := run(context.Background(), []string{"done", "ed3c/noodle#17"}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"done", "ed3c/noodles#17"}); err == nil {
		t.Fatal("foreign completion subject must be rejected")
	}
}

func testCapabilities() Capabilities {
	return Capabilities{
		SchemaVersion: 1,
		Repository:    targetRepository,
		VerificationSurfaces: map[string]Capability{
			"static-analysis":             {Available: true, Carrier: stringPointer("go vet ./...")},
			"unit-test":                   {Available: true, Carrier: stringPointer("go test ./...; pnpm --filter noodle-ui test")},
			"structural":                  {Available: true, Carrier: stringPointer("pnpm generate; git diff --exit-code; sh scripts/lint-arch.sh")},
			"security-advisory":           {Available: false},
			"runtime-oracle":              {Available: true, Carrier: stringPointer("go test ./... -run 'TestNoodlesGitHubTargetConsumer|TestNoodlesDispatchAdmission'")},
			"worktree-execution":          {Available: true, Carrier: stringPointer("noodle-ed3c-v0.1.12 worktree create")},
			"github-actions-verification": {Available: true, Carrier: stringPointer(".github/workflows/test.yml")},
			"provider-handoff":            {Available: true, Carrier: stringPointer("go run ./adapters/noodles-github handoff ed3c/noodle#N")},
			"exact-head-merge":            {Available: true, Carrier: stringPointer(".github/workflows/noodles-land.yml")},
		},
	}
}

func stringPointer(value string) *string { return &value }

func writeFixtureFiles(t *testing.T, dir string) (string, string) {
	t.Helper()
	policyPath := filepath.Join(dir, "github.json")
	capabilitiesPath := filepath.Join(dir, "repo-capabilities.json")
	for path, value := range map[string]any{policyPath: testPolicy(), capabilitiesPath: testCapabilities()} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return policyPath, capabilitiesPath
}
