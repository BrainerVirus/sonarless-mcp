package sonar

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// SyncReport describes what SyncProject changed on the local server.
type SyncReport struct {
	Gate          string   // local gate name now used by the project ("" if none)
	GateSkipped   []string // conditions the local server couldn't take (e.g. plugin metrics)
	Profiles      []string // "lang: local profile name" assigned to the project
	ProfilesSame  []string // languages already identical (same built-in profile and version)
	RuleFailures  []string // "lang: N rules missing locally"
	NewCode       string   // new-code definition copied ("" if left as is)
	RemoteVersion string
	LocalVersion  string
}

type condition struct {
	ID     string `json:"id"`
	Metric string `json:"metric"`
	Op     string `json:"op"`
	Error  string `json:"error"`
}

type gate struct {
	Name       string      `json:"name"`
	IsBuiltIn  bool        `json:"isBuiltIn"`
	Conditions []condition `json:"conditions"`
}

type profile struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Language  string `json:"language"`
	IsBuiltIn bool   `json:"isBuiltIn"`
}

// SyncProject makes the local server judge project like the remote one: it
// copies the remote's quality gate, the quality profiles (rule sets) the
// project uses, and its new-code definition. Copies are named
// "<prefix>: <remote name>" so nothing local or built-in is overwritten.
func SyncProject(ctx context.Context, remote, local *Client, project, projectName, prefix string) (*SyncReport, error) {
	rep := &SyncReport{RemoteVersion: version(ctx, remote), LocalVersion: version(ctx, local)}
	if err := remote.Do(ctx, http.MethodGet, "/api/components/show", url.Values{"component": {project}}, nil); err != nil {
		return nil, fmt.Errorf("project %q not found on the remote server: %w", project, err)
	}
	if err := local.EnsureProject(ctx, project, projectName); err != nil {
		return nil, fmt.Errorf("create %q locally: %w", project, err)
	}
	if err := syncGate(ctx, remote, local, project, prefix, rep); err != nil {
		return nil, fmt.Errorf("quality gate: %w", err)
	}
	if err := syncProfiles(ctx, remote, local, project, prefix, rep); err != nil {
		return nil, fmt.Errorf("quality profiles: %w", err)
	}
	if err := syncNewCode(ctx, remote, local, project, rep); err != nil {
		return nil, fmt.Errorf("new code definition: %w", err)
	}
	return rep, nil
}

func version(ctx context.Context, c *Client) string {
	var r struct{ Version string }
	if c.Do(ctx, http.MethodGet, "/api/system/status", nil, &r) != nil {
		return ""
	}
	return r.Version
}

func syncGate(ctx context.Context, remote, local *Client, project, prefix string, rep *SyncReport) error {
	var byProject struct {
		QualityGate struct{ Name string } `json:"qualityGate"`
	}
	if err := remote.Do(ctx, http.MethodGet, "/api/qualitygates/get_by_project", url.Values{"project": {project}}, &byProject); err != nil {
		return err
	}
	var src gate
	if err := remote.Do(ctx, http.MethodGet, "/api/qualitygates/show", url.Values{"name": {byProject.QualityGate.Name}}, &src); err != nil {
		return err
	}
	name := prefix + ": " + src.Name
	var dst gate
	if err := local.Do(ctx, http.MethodGet, "/api/qualitygates/show", url.Values{"name": {name}}, &dst); err != nil {
		if err := local.Do(ctx, http.MethodPost, "/api/qualitygates/create", url.Values{"name": {name}}, nil); err != nil {
			return err
		}
		// New gates come with default conditions; read them back so they are
		// replaced like any others.
		dst = gate{Name: name}
		_ = local.Do(ctx, http.MethodGet, "/api/qualitygates/show", url.Values{"name": {name}}, &dst)
	}
	// Replace the conditions so the copy matches the remote exactly.
	for _, c := range dst.Conditions {
		if err := local.Do(ctx, http.MethodPost, "/api/qualitygates/delete_condition", url.Values{"id": {c.ID}}, nil); err != nil {
			return err
		}
	}
	for _, c := range src.Conditions {
		err := local.Do(ctx, http.MethodPost, "/api/qualitygates/create_condition", url.Values{
			"gateName": {name}, "metric": {c.Metric}, "op": {c.Op}, "error": {c.Error}}, nil)
		if err != nil {
			rep.GateSkipped = append(rep.GateSkipped, fmt.Sprintf("%s %s %s (%v)", c.Metric, c.Op, c.Error, err))
		}
	}
	if err := local.Do(ctx, http.MethodPost, "/api/qualitygates/select", url.Values{"gateName": {name}, "projectKey": {project}}, nil); err != nil {
		return err
	}
	rep.Gate = name
	return nil
}

func syncProfiles(ctx context.Context, remote, local *Client, project, prefix string, rep *SyncReport) error {
	var rp struct{ Profiles []profile }
	if err := remote.Do(ctx, http.MethodGet, "/api/qualityprofiles/search", url.Values{"project": {project}}, &rp); err != nil {
		return err
	}
	var lp struct{ Profiles []profile }
	if err := local.Do(ctx, http.MethodGet, "/api/qualityprofiles/search", url.Values{"project": {project}}, &lp); err != nil {
		return err
	}
	localByLang := map[string]profile{}
	for _, p := range lp.Profiles {
		localByLang[p.Language] = p
	}
	sameVersion := rep.RemoteVersion != "" && rep.RemoteVersion == rep.LocalVersion
	for _, p := range rp.Profiles {
		// A built-in profile is identical on servers of the same version.
		if p.IsBuiltIn && sameVersion {
			if l, ok := localByLang[p.Language]; ok && l.IsBuiltIn && l.Name == p.Name {
				rep.ProfilesSame = append(rep.ProfilesSame, p.Language)
				continue
			}
		}
		backup, err := remote.raw(ctx, "/api/qualityprofiles/backup", url.Values{"language": {p.Language}, "qualityProfile": {p.Name}})
		if err != nil {
			return fmt.Errorf("back up %s profile %q: %w", p.Language, p.Name, err)
		}
		name := prefix + ": " + p.Name
		backup, err = renameProfile(backup, name)
		if err != nil {
			return err
		}
		failures, err := local.restoreProfile(ctx, backup)
		if err != nil {
			return fmt.Errorf("restore %s profile %q locally: %w", p.Language, name, err)
		}
		if failures > 0 {
			rep.RuleFailures = append(rep.RuleFailures, fmt.Sprintf("%s: %d rules not available locally", p.Language, failures))
		}
		if err := local.Do(ctx, http.MethodPost, "/api/qualityprofiles/add_project", url.Values{
			"language": {p.Language}, "qualityProfile": {name}, "project": {project}}, nil); err != nil {
			return err
		}
		rep.Profiles = append(rep.Profiles, p.Language+": "+name)
	}
	return nil
}

var profileName = regexp.MustCompile(`(?s)(<profile>\s*<name>)(.*?)(</name>)`)

// renameProfile sets the <name> of a profile backup.
func renameProfile(backup []byte, name string) ([]byte, error) {
	var esc bytes.Buffer
	if err := xml.EscapeText(&esc, []byte(name)); err != nil {
		return nil, err
	}
	if !profileName.Match(backup) {
		return nil, fmt.Errorf("unexpected profile backup format")
	}
	return profileName.ReplaceAll(backup, []byte("${1}"+strings.ReplaceAll(esc.String(), "$", "$$")+"${3}")), nil
}

func syncNewCode(ctx context.Context, remote, local *Client, project string, rep *SyncReport) error {
	var r struct {
		Type      string `json:"type"`
		Value     string `json:"value"`
		Inherited bool   `json:"inherited"`
	}
	if err := remote.Do(ctx, http.MethodGet, "/api/new_code_periods/show", url.Values{"project": {project}}, &r); err != nil {
		return nil // older servers: leave the local default
	}
	if r.Type == "" || r.Type == "REFERENCE_BRANCH" || r.Type == "SPECIFIC_ANALYSIS" {
		return nil // depend on branches/analyses that only exist on the remote
	}
	v := url.Values{"project": {project}, "type": {r.Type}}
	if r.Value != "" {
		v.Set("value", r.Value)
	}
	if err := local.Do(ctx, http.MethodPost, "/api/new_code_periods/set", v, nil); err != nil {
		return err
	}
	rep.NewCode = r.Type
	if r.Value != "" {
		rep.NewCode += " " + r.Value
	}
	return nil
}

// raw GETs a non-JSON body (e.g. a profile backup XML).
func (c *Client) raw(ctx context.Context, path string, params url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Pass)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	return b, nil
}

// restoreProfile uploads a profile backup; it returns how many rules the
// local server didn't know (e.g. from plugins it lacks).
func (c *Client) restoreProfile(ctx context.Context, backup []byte) (int, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("backup", "profile.xml")
	if err != nil {
		return 0, err
	}
	if _, err := fw.Write(backup); err != nil {
		return 0, err
	}
	if err := mw.Close(); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/qualityprofiles/restore", &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Pass)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return 0, &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	var r struct {
		RuleFailures int `json:"ruleFailures"`
	}
	_ = json.Unmarshal(b, &r)
	return r.RuleFailures, nil
}
