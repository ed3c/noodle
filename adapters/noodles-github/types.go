package main

import "encoding/json"

const (
	targetRepository        = "ed3c/noodle"
	sourceRepository        = "ed3c/noodles"
	defaultBranch           = "main"
	dispatchEventType       = "noodles-execution"
	crossRepositoryAdmitted = "TARGET_INSTALLATION_AND_TOKEN_READBACK_RECORDED"
	authorizationMarker     = "<!-- noodles-execution-authorization:v1 -->\n"
	targetExecutionSkill    = "noodles-issue-execute"
)

type DispatchPayload struct {
	SourceRepository  string   `json:"source_repository"`
	Target            string   `json:"target"`
	Subject           string   `json:"subject"`
	SubjectBodySHA256 string   `json:"subject_body_sha256"`
	BaseSHA           string   `json:"base_sha"`
	Runtime           string   `json:"runtime"`
	Evidence          string   `json:"evidence"`
	WriteBoundary     []string `json:"write_boundary"`
}

type Authorization struct {
	SchemaVersion    int             `json:"schema_version"`
	DispatchIdentity string          `json:"dispatch_identity"`
	Sender           string          `json:"sender"`
	Declaration      DispatchPayload `json:"declaration"`
}

type Policy struct {
	SchemaVersion              int      `json:"schema_version"`
	Repository                 string   `json:"repository"`
	AllowedRepositories        []string `json:"allowed_repositories"`
	DefaultBranch              string   `json:"default_branch"`
	CrossRepositoryStatus      string   `json:"cross_repository_status"`
	RepositoryDispatchSender   string   `json:"repository_dispatch_sender"`
	AuthorizationCommentAuthor string   `json:"authorization_comment_author"`
}

type Capabilities struct {
	SchemaVersion        int                   `json:"schema_version"`
	Repository           string                `json:"repository"`
	VerificationSurfaces map[string]Capability `json:"verification_surfaces"`
}

type Capability struct {
	Available bool    `json:"available"`
	Carrier   *string `json:"carrier"`
}

type Repository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type GitRef struct {
	Object GitObject `json:"object"`
}

type GitObject struct {
	SHA string `json:"sha"`
}

type Issue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	State       string          `json:"state"`
	Body        string          `json:"body"`
	PullRequest json.RawMessage `json:"pull_request,omitempty"`
}

type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User User   `json:"user"`
}

type User struct {
	Login string `json:"login"`
}

type AdmissionResult struct {
	Status           string `json:"status"`
	DispatchIdentity string `json:"dispatch_identity"`
	CommentID        int64  `json:"comment_id"`
}

type BacklogItem struct {
	ID             string        `json:"id"`
	Title          string        `json:"title"`
	Status         string        `json:"status"`
	Body           string        `json:"body"`
	Repository     string        `json:"repository"`
	IssueNumber    int           `json:"issue_number"`
	Authorization  Authorization `json:"authorization"`
	ExecutionSkill string        `json:"execution_skill"`
}

type Diagnostic struct {
	Subject string `json:"subject"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
