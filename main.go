package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/build/gerrit"
)

type Sync struct {
	GerritURL string // Base URL for Gerrit instance.

	Owner     string // GitHub owner (user or organization)
	AuthToken string // GitHub authentication token (user:hex).

	gerrit *gerrit.Client
}

type Change struct {
	*gerrit.ChangeInfo
	*PullRequest
}

// GitHub API

type PullRequest struct {
	Number int
	Head   GitHubRevision
	Base   GitHubRevision
}

type GitHubRevision struct {
	Ref  string
	SHA  string
	Repo struct {
		Name string `json:"full_name"`
	}
}

type GitHubStatus struct {
	Context     string
	State       string
	Description string
	Target      string `json:"target_url"`
}

func main() {
	s := Sync{
		GerritURL: "https://upspin-review.googlesource.com",
		Owner:     "adg",
		AuthToken: "adg:REDACTED",
	}

	if err := s.run(); err != nil {
		log.Fatal(err)
	}
}

func (s *Sync) run() error {
	auth := gerrit.GitCookiesAuth()
	s.gerrit = gerrit.NewClient(s.GerritURL, auth)

	root, err := ioutil.TempDir("", "gitsync")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	changes := map[string]*Change{}

	cis, err := s.gerritChanges()
	if err != nil {
		return err
	}
	for _, ci := range cis {
		changes[ci.ChangeID] = &Change{
			ChangeInfo: ci,
		}
	}

	repos, err := s.githubRepos()
	if err != nil {
		return err
	}
	for _, repo := range repos {
		prs, err := s.pullRequests(repo)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if !isGerritChange(pr.Head.Ref) {
				continue
			}
			c, ok := changes[pr.Head.Ref]
			if !ok {
				c = &Change{}
				changes[pr.Head.Ref] = c
			}
			c.PullRequest = pr
		}
	}

	for _, c := range changes {
		switch {
		case c.PullRequest == nil && c.ChangeInfo != nil:
			// Sync branch and create pull request.
			ci := c.ChangeInfo
			log.Printf("Gerrit change %v needs corresponding pull request. Creating one.", ci.ChangeID)
			dir := filepath.Join(root, ci.Project)
			if err := s.syncBranch(dir, ci); err != nil {
				return err
			}
			if err := s.createPullRequest(ci); err != nil {
				return err
			}
		case c.PullRequest != nil && c.ChangeInfo != nil:
			ci := c.ChangeInfo
			if c.PullRequest.Head.SHA == c.ChangeInfo.CurrentRevision {
				// Already in sync; nothing to do.
				log.Printf("Gerrit change %v already synced with pull request.", ci.ChangeID)
				if err := s.syncComments(c); err != nil {
					return err
				}
				break
			}
			// Sync branch.
			log.Printf("Gerrit change %v needs sync with pull request. Syncing.", ci.ChangeID)
			dir := filepath.Join(root, ci.Project)
			if err := s.syncBranch(dir, ci); err != nil {
				return err
			}
		case c.PullRequest != nil && c.ChangeInfo == nil:
			// Close pull request and delete branch.
			pr := c.PullRequest
			log.Printf("Pull request %v has no corresponding Gerrit change. Closing.", pr.Number)
			if err := s.closePullRequest(pr); err != nil {
				return err
			}
			repo := strings.SplitN(pr.Head.Repo.Name, "/", 2)[1]
			dir := filepath.Join(root, repo)
			if err := s.deleteBranch(dir, repo, pr.Head.Ref); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Sync) gerritChanges() ([]*gerrit.ChangeInfo, error) {
	ctx := context.Background()
	opt := gerrit.QueryChangesOpt{Fields: []string{"CURRENT_REVISION", "MESSAGES"}}
	return s.gerrit.QueryChanges(ctx, "is:open", opt)
}

func (s *Sync) syncBranch(dir string, c *gerrit.ChangeInfo) error {
	if err := s.clone(dir, c.Project); err != nil {
		return err
	}
	// Switch to the branch for this change.
	if err := git(dir, "checkout", c.ChangeID); err != nil {
		// Branch doesn't exist for this change; create one.
		err2 := git(dir, "checkout", "-b", c.ChangeID)
		if err2 != nil {
			return err
		}
	}
	// Reset the branch to the current change head.
	src := s.GerritURL + "/" + c.Project
	ref := c.Revisions[c.CurrentRevision].Ref
	if err := git(dir, "fetch", src, ref); err != nil {
		return err
	}
	if err := git(dir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return err
	}
	// Push the branch to GitHub.
	dest := "https://" + s.AuthToken + "@github.com/" + s.Owner + "/" + c.Project
	return git(dir, "push", "-f", dest, c.ChangeID)
}

func (s *Sync) deleteBranch(dir, repo, id string) error {
	if err := s.clone(dir, repo); err != nil {
		return err
	}
	// Delete the remote branch.
	dest := "https://" + s.AuthToken + "@github.com/" + s.Owner + "/" + repo
	if err := git(dir, "push", "--delete", dest, id); err != nil {
		return err
	}
	// Delete the local branch.
	git(dir, "branch", "-D", id) // Ignore errors.
	return nil
}

func (s *Sync) clone(dir, project string) error {
	if fi, err := os.Stat(dir); err != nil && !os.IsNotExist(err) {
		return err
	} else if err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("clone destination is not a directory: %v", dir)
		}
		// We're already cloned here; so just do a pull to make sure we're up to date.
		if err := git(dir, "checkout", "master"); err != nil {
			return nil
		}
		return git(dir, "pull")
	}
	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}
	url := s.GerritURL + "/" + project
	if err := git(dir, "clone", url, dir); err != nil {
		os.RemoveAll(dir)
		return err
	}
	return git(dir, "checkout", "master")
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

func (s *Sync) pullRequests(repo string) (prs []*PullRequest, err error) {
	return prs, s.gitHub("repos/"+s.Owner+"/"+repo+"/pulls", nil, &prs)
}

func (s *Sync) createPullRequest(ci *gerrit.ChangeInfo) error {
	payload := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}{
		Title: ci.Subject,
		Body:  "Automatically created pull request. **Do not review or merge this PR.**",
		Head:  ci.ChangeID,
		Base:  "master",
	}
	return s.gitHub("repos/"+s.Owner+"/"+ci.Project+"/pulls", payload, nil)
}

func (s *Sync) closePullRequest(pr *PullRequest) error {
	payload := struct {
		State string `json:"state"`
	}{"closed"}
	return s.gitHub("repos/"+pr.Head.Repo.Name+"/pulls/"+fmt.Sprint(pr.Number), payload, nil)
}

func (s *Sync) syncComments(c *Change) error {
	pr := c.PullRequest
	ci := c.ChangeInfo

	// Fetch Pull Request statuses.
	var statuses []*GitHubStatus
	err := s.gitHub("repos/"+pr.Head.Repo.Name+"/commits/"+pr.Head.SHA+"/statuses", nil, &statuses)
	if err != nil {
		return err
	}

	ctx := context.Background()
	for _, stat := range statuses {
		if stat.Context != "continuous-integration/travis-ci/pr" {
			continue
		}
		if stat.State != "success" && stat.State != "failure" {
			continue
		}
		msg := fmt.Sprintf("%v: %v", stat.Description, stat.Target)

		// Check whether an equivalent Gerrit comment exists.
		found := false
		for _, m := range ci.Messages {
			if strings.Contains(m.Message, msg) {
				found = true
				break
			}
		}
		if !found {
			// If no such comment exists, post it.
			err = s.gerrit.SetReview(ctx, ci.ChangeID, ci.CurrentRevision, gerrit.ReviewInput{
				Message: msg,
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Sync) githubRepos() ([]string, error) {
	var result []struct {
		Name string
	}
	err := s.gitHub("users/"+s.Owner+"/repos", nil, &result)
	if err != nil {
		return nil, err
	}
	var repos []string
	for _, r := range result {
		repos = append(repos, r.Name)
	}
	return repos, nil
}

func (s *Sync) gitHub(path string, payload, result interface{}) error {
	url := "https://" + s.AuthToken + "@api.github.com/" + path

	var r *http.Response
	var err error
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		r, err = http.Post(url, "application/json", bytes.NewReader(b))
	} else {
		r, err = http.Get(url)
	}
	if err != nil {
		return err
	}
	b, err := ioutil.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return err
	}
	if r.StatusCode/100 != 2 {
		return errors.New(r.Status)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(b, result)
}

func isGerritChange(id string) bool {
	return strings.HasPrefix(id, "I")
}
