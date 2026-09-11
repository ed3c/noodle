package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

var (
	sha40Pattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha64Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	subjectPattern = regexp.MustCompile(`^ed3c/noodle#([1-9][0-9]*)$`)
	markerPattern  = regexp.MustCompile(`(?m)^<!-- noodles-([a-z-]+): ([^\r\n]*?) -->$`)
)

type issueContract struct {
	Target        string
	Subject       string
	State         string
	Executor      string
	Runtime       string
	Evidence      string
	WriteBoundary []string
}

func parseIssueContract(body string, number int) (issueContract, error) {
	values := make(map[string]string)
	for _, match := range markerPattern.FindAllStringSubmatch(body, -1) {
		if _, exists := values[match[1]]; exists {
			return issueContract{}, fmt.Errorf("duplicate noodles-%s marker", match[1])
		}
		values[match[1]] = strings.TrimSpace(match[2])
	}
	boundary := splitBoundary(values["write-boundary"])
	expectedSubject := fmt.Sprintf("%s#%d", targetRepository, number)
	contract := issueContract{
		Target: values["target"], Subject: expectedSubject, State: values["state"],
		Executor: values["executor"], Runtime: values["runtime"], Evidence: values["evidence"],
		WriteBoundary: boundary,
	}
	if contract.Target != targetRepository {
		return issueContract{}, fmt.Errorf("Issue target is %q, want %q", contract.Target, targetRepository)
	}
	if declared := values["subject"]; declared != expectedSubject {
		return issueContract{}, fmt.Errorf("Issue subject marker is %q, want %q", declared, expectedSubject)
	}
	if contract.State != "ready" || contract.Executor != "local-noodle" {
		return issueContract{}, fmt.Errorf("Issue route is state=%q executor=%q, want ready/local-noodle", contract.State, contract.Executor)
	}
	if contract.Runtime == "" || contract.Evidence == "" || len(contract.WriteBoundary) == 0 {
		return issueContract{}, fmt.Errorf("Issue has incomplete runtime, evidence, or write-boundary markers")
	}
	return contract, nil
}

func splitBoundary(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			return nil
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

func parseSubject(subject string) (int, error) {
	match := subjectPattern.FindStringSubmatch(subject)
	if len(match) != 2 {
		return 0, fmt.Errorf("subject %q is not an exact %s Issue", subject, targetRepository)
	}
	number, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("parse subject %q: %w", subject, err)
	}
	return number, nil
}

func DispatchIdentity(payload DispatchPayload) (string, error) {
	values := map[string]any{
		"base_sha": payload.BaseSHA, "evidence": payload.Evidence, "runtime": payload.Runtime,
		"source_repository": payload.SourceRepository, "subject": payload.Subject,
		"subject_body_sha256": payload.SubjectBodySHA256, "target": payload.Target,
		"write_boundary": payload.WriteBoundary,
	}
	keys := []string{"base_sha", "evidence", "runtime", "source_repository", "subject", "subject_body_sha256", "target", "write_boundary"}
	var canonical bytes.Buffer
	canonical.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			canonical.WriteString(", ")
		}
		keyJSON, err := encodeCanonicalJSON(key)
		if err != nil {
			return "", fmt.Errorf("encode dispatch identity key: %w", err)
		}
		canonical.Write(keyJSON)
		canonical.WriteString(": ")
		if key == "write_boundary" {
			canonical.WriteByte('[')
			for boundaryIndex, entry := range payload.WriteBoundary {
				if boundaryIndex > 0 {
					canonical.WriteString(", ")
				}
				entryJSON, err := encodeCanonicalJSON(entry)
				if err != nil {
					return "", fmt.Errorf("encode dispatch write boundary: %w", err)
				}
				canonical.Write(entryJSON)
			}
			canonical.WriteByte(']')
			continue
		}
		encoded, err := encodeCanonicalJSON(values[key])
		if err != nil {
			return "", fmt.Errorf("encode dispatch identity: %w", err)
		}
		canonical.Write(encoded)
	}
	canonical.WriteByte('}')
	sum := sha256.Sum256(canonical.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func encodeCanonicalJSON(value any) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}), nil
}

func FormatAuthorization(receipt Authorization) string {
	data, _ := json.Marshal(receipt)
	return authorizationMarker + string(data)
}

func ParseAuthorization(body string) (Authorization, error) {
	var receipt Authorization
	if !strings.HasPrefix(body, authorizationMarker) {
		return receipt, fmt.Errorf("authorization marker is absent")
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(body, authorizationMarker)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("decode authorization: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return receipt, fmt.Errorf("decode authorization trailing data: %w", err)
	}
	if receipt.SchemaVersion != 1 || !sha64Pattern.MatchString(receipt.DispatchIdentity) {
		return receipt, fmt.Errorf("authorization schema or dispatch identity is invalid")
	}
	derived, err := DispatchIdentity(receipt.Declaration)
	if err != nil || derived != receipt.DispatchIdentity {
		return receipt, fmt.Errorf("authorization dispatch identity disagrees with its declaration")
	}
	return receipt, nil
}
