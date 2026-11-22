package proxy

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type demandProject struct {
	ID     string
	Title  string // Empty between a linked project's clone and create calls.
	Remote string // Credential-free URL used by the worker's Git installation.
}

func (d *dispatcher) loadDemandProjects() error {
	demand := d.disk.OnDemand
	legacy := demand.Projects == nil
	if legacy {
		demand.Projects = map[string]demandProject{}
		if p := demand.Project; p != nil {
			demand.Projects[p.ID] = *p
			demand.Project = nil
		}
	} else if demand.Project != nil {
		return errors.New("conflicting saved dispatch project catalogs")
	}
	seen := make(map[string]bool, len(demand.Projects))
	for id, p := range demand.Projects {
		key := p.repositoryKey()
		if !validDispatchThreadID(id) || id != p.ID || demandRemote(p.Remote) == "" || seen[key] {
			return errors.New("invalid saved dispatch project catalog")
		}
		seen[key] = true
	}
	for _, route := range demand.Routes {
		if demand.Projects[route.ProjectID].Title == "" {
			return errors.New("a saved dispatch route has no registered project")
		}
	}
	if legacy {
		return d.saveDispatchState(d.disk)
	}
	return nil
}

var repositoryPath = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_.-]{1,100}$`)

func demandRemote(raw string) string {
	if path, ok := strings.CutPrefix(raw, "git@github.com:"); ok {
		if safeRepositoryPath(path) {
			return raw
		}
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != "github.com" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.RawPath != "" ||
		!safeRepositoryPath(strings.TrimPrefix(u.Path, "/")) {
		return ""
	}
	if u.Scheme == "https" && u.User == nil {
		return raw
	}
	if u.Scheme == "ssh" && u.User != nil && u.User.String() == "git" {
		return "git@github.com:" + strings.TrimPrefix(u.Path, "/")
	}
	return ""
}

func safeRepositoryPath(path string) bool {
	repo := strings.TrimSuffix(path, ".git")
	return repositoryPath.MatchString(path) && !strings.HasSuffix(repo, "/.") && !strings.HasSuffix(repo, "/..") && !strings.HasSuffix(repo, "/")
}

// Native T3 clone calls can use either a URL or a GitHub owner/repository.
func projectRemote(args map[string]any) string {
	raw, _ := args["remoteUrl"].(string)
	repo, _ := args["repository"].(string)
	provider, _ := args["provider"].(string)
	protocol, _ := args["protocol"].(string)
	if provider != "" && provider != "github" {
		return ""
	}
	if raw != "" {
		return demandRemote(raw)
	}
	if !safeRepositoryPath(repo) {
		return ""
	}
	path := strings.TrimSuffix(repo, ".git") + ".git"
	switch protocol {
	case "", "auto", "https":
		return demandRemote("https://github.com/" + path)
	case "ssh":
		return demandRemote("git@github.com:" + path)
	default:
		return ""
	}
}

func (p demandProject) repositoryPath() string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(p.Remote, "git@github.com:"), "https://github.com/"), ".git")
}

func (p demandProject) repositoryKey() string {
	return "github.com/" + strings.ToLower(p.repositoryPath())
}

func (d *dispatcher) projectPath(id string) string {
	return filepath.Join(d.onDemand.ProjectRoot, demandKey("project", id))
}

// A Desktop can send a new project ID when the repository already exists.
// Keep the original ID, Git transport and display name. Never retarget an ID.
func (d *dispatcher) rememberProject(p demandProject) (demandProject, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	projects := d.disk.OnDemand.Projects
	if existing, ok := projects[p.ID]; ok && existing.repositoryKey() != p.repositoryKey() {
		return demandProject{}, errors.New("this project ID already belongs to another repository")
	}
	for _, existing := range projects {
		if existing.repositoryKey() == p.repositoryKey() {
			if existing.Title != "" || p.Title == "" {
				return existing, nil
			}
			p.ID, p.Remote = existing.ID, existing.Remote
			break
		}
	}
	next := d.disk
	demand := *next.OnDemand
	demand.Projects = maps.Clone(projects)
	demand.Projects[p.ID] = p
	next.OnDemand = &demand
	if err := d.saveDispatchState(next); err != nil {
		return demandProject{}, errors.New("could not save the project")
	}
	d.disk = next
	return p, nil
}

func validProjectTitle(title string) bool { return strings.TrimSpace(title) != "" && len(title) <= 256 }

func (d *dispatcher) registerRepository(ctx context.Context, payload map[string]any) (map[string]any, error) {
	id, _ := payload["projectId"].(string)
	title, _ := payload["title"].(string)
	remote := projectRemote(payload)
	if !validDispatchThreadID(id) || !validProjectTitle(title) || remote == "" {
		return nil, errors.New("choose a project title and a GitHub repository without embedded credentials")
	}
	project, err := d.rememberProject(demandProject{ID: id, Remote: remote})
	if err != nil {
		return nil, err
	}
	path := d.projectPath(project.ID)
	if _, err := d.registerControllerProject(ctx, project, title); err != nil {
		return nil, err
	}
	return map[string]any{"projectId": project.ID, "cwd": path, "remoteUrl": project.Remote, "repository": nil}, nil
}

// A clone becomes launchable only after the native controller confirms the
// registration. If a reply or save is lost, the same command ID recovers it.
func (d *dispatcher) registerControllerProject(ctx context.Context, project demandProject, title string) (map[string]any, error) {
	if project.Title == "" {
		project.Title = title
	}
	path := d.projectPath(project.ID)
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, errors.New("could not prepare virtual project directory")
	}
	row, err := d.seedProject(ctx, d.onDemand.Controller, project, path)
	if err != nil {
		return nil, err
	}
	project.Title, _ = row["title"].(string)
	if !validProjectTitle(project.Title) {
		return nil, errors.New("T3 did not confirm the registered project title")
	}
	_, err = d.rememberProject(project)
	return row, err
}

func (d *dispatcher) seedProject(ctx context.Context, worker DispatchWorker, p demandProject, path string) (map[string]any, error) {
	req := map[string]any{"_tag": "Request", "id": "dispatch-project", "tag": "projects.mutate", "headers": []any{},
		"payload": map[string]any{"type": "project.create", "projectId": p.ID, "title": p.Title, "workspaceRoot": path,
			"commandId": "dispatch-project-" + demandKey(p.ID)}}
	var project map[string]any
	err := d.rpcTo(ctx, worker, req, nil, func(reply map[string]any) error {
		exit, _ := reply["exit"].(map[string]any)
		project, _ = exit["value"].(map[string]any)
		if exit["_tag"] != "Success" || project["id"] != p.ID || project["workspaceRoot"] != path {
			return errors.New("T3 could not register the dispatch project at its expected path")
		}
		return nil
	})
	return project, err
}

// The controller has no Git checkout. Supply the same canonical repository
// identity a native T3 project list or shell would expose for the worker.
func (d *dispatcher) decorateProject(row map[string]any) {
	id, _ := row["id"].(string)
	d.mu.Lock()
	project, ok := d.disk.OnDemand.Projects[id]
	d.mu.Unlock()
	if !ok || project.Title == "" {
		return
	}
	path := project.repositoryPath()
	owner, name, _ := strings.Cut(path, "/")
	row["repositoryIdentity"] = map[string]any{
		"canonicalKey": project.repositoryKey(), "rootPath": d.projectPath(id),
		"locator":     map[string]string{"source": "git-remote", "remoteName": "origin", "remoteUrl": project.Remote},
		"displayName": path, "provider": "github", "owner": owner, "name": name, "webUrl": "https://github.com/" + path,
	}
	row["defaultThreadEnvMode"] = "local"
}

func (d *dispatcher) peerProject(ctx context.Context, tool peerTool) (map[string]any, error) {
	args := tool.Arguments
	if tool.Name == "t3_project_clone" {
		remote := projectRemote(args)
		path, _ := args["destinationPath"].(string)
		if remote == "" || strings.TrimSpace(path) == "" {
			return nil, errors.New("choose a destination and a GitHub repository without embedded credentials")
		}
		p := demandProject{Remote: remote}
		p.ID = demandKey("linked-project", p.repositoryKey())
		p, err := d.rememberProject(p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"cwd": d.projectPath(p.ID), "remoteUrl": p.Remote, "repository": nil}, nil
	}
	// T3 returns cwd from clone. Accept that exact virtual directory only;
	// a linked caller cannot adopt or create an arbitrary controller path.
	path, _ := args["workspaceRoot"].(string)
	title, _ := args["title"].(string)
	if !validProjectTitle(title) || args["scripts"] != nil || args["defaultModelSelection"] != nil {
		return nil, errors.New("register a cloned project with its title and returned workspaceRoot")
	}
	d.mu.Lock()
	var project demandProject
	for id, candidate := range d.disk.OnDemand.Projects {
		if path == d.projectPath(id) {
			project = candidate
			break
		}
	}
	d.mu.Unlock()
	if project.ID == "" {
		return nil, errors.New("use the cwd returned by t3_project_clone as the workspaceRoot")
	}
	return d.registerControllerProject(ctx, project, title)
}
