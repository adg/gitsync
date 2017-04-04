package main

import (
	"bytes"
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
)

type Sync struct {
	Host string // Gerrit Host

	Owner     string // GitHub owner (user or organization)
	AuthToken string // GitHub authentication token (user:hex).
}

type Change struct {
	GerritChange *GerritChange
	PullRequest  *PullRequest
}

// Gerrit API

type GerritChange struct {
	Project   string
	ID        string `json:"change_id"`
	Revision  string `json:"current_revision"`
	Revisions map[string]GerritRevision
	Subject   string
}

type GerritRevision struct {
	Ref string
}

func (c *GerritChange) Ref() string {
	return c.Revisions[c.Revision].Ref
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
		Host:      "upspin",
		Owner:     "adg",
		AuthToken: "adg:REDACTED",
	}

	if err := s.run(); err != nil {
		log.Fatal(err)
	}
}

func (s *Sync) run() error {
	root, err := ioutil.TempDir("", "gitsync")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	changes := map[string]*Change{}

	gChanges, err := s.gerritChanges()
	if err != nil {
		return err
	}
	for _, gc := range gChanges {
		changes[gc.ID] = &Change{
			GerritChange: gc,
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
		case c.PullRequest == nil && c.GerritChange != nil:
			// Sync branch and create pull request.
			gc := c.GerritChange
			log.Printf("Gerrit change %v needs corresponding pull request. Creating one.", gc.ID)
			dir := filepath.Join(root, gc.Project)
			if err := s.syncBranch(dir, gc); err != nil {
				return err
			}
			if err := s.createPullRequest(gc); err != nil {
				return err
			}
		case c.PullRequest != nil && c.GerritChange != nil:
			gc := c.GerritChange
			if c.PullRequest.Head.SHA == c.GerritChange.Revision {
				// Already in sync; nothing to do.
				log.Printf("Gerrit change %v already synced with pull request.", gc.ID)
				if err := s.syncComments(c); err != nil {
					return err
				}
				break
			}
			// Sync branch.
			log.Printf("Gerrit change %v needs sync with pull request. Syncing.", gc.ID)
			dir := filepath.Join(root, gc.Project)
			if err := s.syncBranch(dir, gc); err != nil {
				return err
			}
		case c.PullRequest != nil && c.GerritChange == nil:
			// Close pull request and delete branch.
			pr := c.PullRequest
			log.Printf("Pull request %v has no corresponding Gerrit change. Closing.", pr.Number)
			if err := s.syncComments(c); err != nil {
				return err
			}
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

func (s *Sync) gerritChanges() ([]*GerritChange, error) {
	url := "https://" + s.Host + "-review.googlesource.com/changes/?q=is:open&o=CURRENT_REVISION"

	body, err := ioutil.ReadFile("changes.json") // xxx
	if err != nil {

		r, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		body, err = ioutil.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return nil, err
		}
		if r.StatusCode != http.StatusOK {
			return nil, errors.New(r.Status)
		}
		body = bytes.TrimPrefix(body, []byte(")]}'\n"))

		ioutil.WriteFile("changes.json", body, 0644) // xxx
	}

	var changes []*GerritChange
	if err := json.Unmarshal(body, &changes); err != nil {
		return nil, err
	}

	return changes, nil
}

func (s *Sync) syncBranch(dir string, c *GerritChange) error {
	if err := s.clone(dir, c.Project); err != nil {
		return err
	}
	// Switch to the branch for this change.
	if err := git(dir, "checkout", c.ID); err != nil {
		// Branch doesn't exist for this change; create one.
		err2 := git(dir, "checkout", "-b", c.ID)
		if err2 != nil {
			return err
		}
	}
	// Reset the branch to the current change head.
	src := "https://" + s.Host + ".googlesource.com/" + c.Project
	if err := git(dir, "fetch", src, c.Ref()); err != nil {
		return err
	}
	if err := git(dir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return err
	}
	// Push the branch to GitHub.
	dest := "https://" + s.AuthToken + "@github.com/" + s.Owner + "/" + c.Project
	return git(dir, "push", "-f", dest, c.ID)
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
	url := "https://" + s.Host + ".googlesource.com/" + project
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

func (s *Sync) createPullRequest(gc *GerritChange) error {
	payload := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}{
		Title: gc.Subject,
		Body:  "Automatically created pull request. Do not review.",
		Head:  gc.ID,
		Base:  "master",
	}
	return s.gitHub("repos/"+s.Owner+"/"+gc.Project+"/pulls", payload, nil)
}

func (s *Sync) closePullRequest(pr *PullRequest) error {
	payload := struct {
		State string `json:"state"`
	}{"closed"}
	return s.gitHub("repos/"+pr.Head.Repo.Name+"/pulls/"+fmt.Sprint(pr.Number), payload, nil)
}

func (s *Sync) syncComments(c *Change) error {
	pr := c.PullRequest
	var statuses []*GitHubStatus
	err := s.gitHub("repos/"+pr.Head.Repo.Name+"/commits/"+pr.Head.SHA+"/statuses", nil, &statuses)
	if err != nil {
		return err
	}
	for _, stat := range statuses {
		if stat.Context != "continuous-integration/travis-ci/pr" {
			continue
		}
		if stat.State != "success" && stat.State != "failure" {
			continue
		}
		msg := fmt.Sprintf("%v: %v", stat.Description, stat.Target)
		log.Println(msg)
		// TODO(adg): check whether an equivalent Gerrit comment
		// exists, and if not, post one.
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
