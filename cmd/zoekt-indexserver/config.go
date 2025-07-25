// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

type ConfigEntry struct {
	GithubUser             string
	GithubOrg              string
	BitBucketServerProject string
	GitHubURL              string
	GitilesURL             string
	CGitURL                string
	BitBucketServerURL     string
	DisableTLS             bool
	CredentialPath         string
	ProjectType            string
	Name                   string
	Exclude                string
	GitLabURL              string
	OnlyPublic             bool
	GerritApiURL           string
	Topics                 []string
	ExcludeTopics          []string
	Active                 bool
	NoArchived             bool
	KeepDeleted            bool
	GerritFetchMetaConfig  bool
	GerritRepoNameFormat   string
	ExcludeUserRepos       bool
}

func randomize(entries []ConfigEntry) []ConfigEntry {
	perm := rand.Perm(len(entries))

	var shuffled []ConfigEntry
	for _, i := range perm {
		shuffled = append(shuffled, entries[i])
	}

	return shuffled
}

func isHTTP(u string) bool {
	asURL, err := url.Parse(u)
	return err == nil && (asURL.Scheme == "http" || asURL.Scheme == "https")
}

func readConfigURL(u string) ([]ConfigEntry, error) {
	var body []byte
	var readErr error

	if isHTTP(u) {
		rep, err := http.Get(u)
		if err != nil {
			return nil, err
		}
		defer rep.Body.Close()

		body, readErr = io.ReadAll(rep.Body)
	} else {
		body, readErr = os.ReadFile(u)
	}

	if readErr != nil {
		return nil, readErr
	}

	var result []ConfigEntry
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func watchFile(path string) (<-chan struct{}, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := watcher.Add(filepath.Dir(path)); err != nil {
		return nil, err
	}

	out := make(chan struct{}, 1)
	go func() {
		var last time.Time
		for {
			select {
			case <-watcher.Events:
				fi, err := os.Stat(path)
				if err == nil && fi.ModTime() != last {
					out <- struct{}{}
					last = fi.ModTime()
				}
			case err := <-watcher.Errors:
				if err != nil {
					log.Printf("watcher error: %v", err)
				}
			}
		}
	}()
	return out, nil
}

func periodicMirrorFile(repoDir string, opts *Options, pendingRepos chan<- string) {
	ticker := time.NewTicker(opts.mirrorInterval)

	var watcher <-chan struct{}
	if !isHTTP(opts.mirrorConfigFile) {
		var err error
		watcher, err = watchFile(opts.mirrorConfigFile)
		if err != nil {
			log.Printf("watchFile(%q): %v", opts.mirrorConfigFile, err)
		}
	}

	var lastCfg []ConfigEntry
	for {
		cfg, err := readConfigURL(opts.mirrorConfigFile)
		if err != nil {
			log.Printf("readConfig(%s): %v", opts.mirrorConfigFile, err)
		} else {
			lastCfg = cfg
		}

		executeMirror(lastCfg, repoDir, pendingRepos)

		select {
		case <-watcher:
			log.Printf("mirror config %s changed", opts.mirrorConfigFile)
		case <-ticker.C:
		}
	}
}

func executeMirror(cfg []ConfigEntry, repoDir string, pendingRepos chan<- string) {
	// Randomize the ordering in which we query
	// things. This is to ensure that quota limits don't
	// always hit the last one in the list.
	cfg = randomize(cfg)
	for _, c := range cfg {
		var cmd *exec.Cmd
		if c.GitHubURL != "" || c.GithubUser != "" || c.GithubOrg != "" {
			cmd = exec.Command("zoekt-mirror-github",
				"-dest", repoDir)
			if c.GitHubURL != "" {
				cmd.Args = append(cmd.Args, "-url", c.GitHubURL)
			}
			if c.GithubUser != "" {
				cmd.Args = append(cmd.Args, "-user", c.GithubUser)
			} else if c.GithubOrg != "" {
				cmd.Args = append(cmd.Args, "-org", c.GithubOrg)
			}
			if c.Name != "" {
				cmd.Args = append(cmd.Args, "-name", c.Name)
			}
			if c.Exclude != "" {
				cmd.Args = append(cmd.Args, "-exclude", c.Exclude)
			}
			if c.CredentialPath != "" {
				cmd.Args = append(cmd.Args, "-token", c.CredentialPath)
			}
			for _, topic := range c.Topics {
				cmd.Args = append(cmd.Args, "-topic", topic)
			}
			for _, topic := range c.ExcludeTopics {
				cmd.Args = append(cmd.Args, "-exclude_topic", topic)
			}
			if c.NoArchived {
				cmd.Args = append(cmd.Args, "-no_archived")
			}
			if !c.KeepDeleted {
				cmd.Args = append(cmd.Args, "-delete")
			}
		} else if c.GitilesURL != "" {
			cmd = exec.Command("zoekt-mirror-gitiles",
				"-dest", repoDir, "-name", c.Name)
			if c.Exclude != "" {
				cmd.Args = append(cmd.Args, "-exclude", c.Exclude)
			}
			cmd.Args = append(cmd.Args, c.GitilesURL)
		} else if c.CGitURL != "" {
			cmd = exec.Command("zoekt-mirror-gitiles",
				"-type", "cgit",
				"-dest", repoDir, "-name", c.Name)
			if c.Exclude != "" {
				cmd.Args = append(cmd.Args, "-exclude", c.Exclude)
			}
			cmd.Args = append(cmd.Args, c.CGitURL)
		} else if c.BitBucketServerURL != "" {
			cmd = exec.Command("zoekt-mirror-bitbucket-server",
				"-dest", repoDir, "-url", c.BitBucketServerURL)
			if c.BitBucketServerProject != "" {
				cmd.Args = append(cmd.Args, "-project", c.BitBucketServerProject)
			}
			if c.DisableTLS {
				cmd.Args = append(cmd.Args, "-disable-tls")
			}
			if c.ProjectType != "" {
				cmd.Args = append(cmd.Args, "-type", c.ProjectType)
			}
			if c.Name != "" {
				cmd.Args = append(cmd.Args, "-name", c.Name)
			}
			if c.Exclude != "" {
				cmd.Args = append(cmd.Args, "-exclude", c.Exclude)
			}
			if c.CredentialPath != "" {
				cmd.Args = append(cmd.Args, "-credentials", c.CredentialPath)
			}
			if !c.KeepDeleted {
				cmd.Args = append(cmd.Args, "-delete")
			}
		} else if c.GitLabURL != "" {
			cmd = exec.Command("zoekt-mirror-gitlab",
				"-dest", repoDir, "-url", c.GitLabURL)
			if c.Name != "" {
				cmd.Args = append(cmd.Args, "-name", c.Name)
			}
			if c.Exclude != "" {
				cmd.Args = append(cmd.Args, "-exclude", c.Exclude)
			}
			if c.OnlyPublic {
				cmd.Args = append(cmd.Args, "-public")
			}
			if c.ExcludeUserRepos {
				cmd.Args = append(cmd.Args, "-exclude_user")
			}
			if c.CredentialPath != "" {
				cmd.Args = append(cmd.Args, "-token", c.CredentialPath)
			}
			if c.NoArchived {
				cmd.Args = append(cmd.Args, "-no_archived")
			}
			if !c.KeepDeleted {
				cmd.Args = append(cmd.Args, "-delete")
			}
		} else if c.GerritApiURL != "" {
			cmd = exec.Command("zoekt-mirror-gerrit",
				"-dest", repoDir)
			if c.CredentialPath != "" {
				cmd.Args = append(cmd.Args, "-http-credentials", c.CredentialPath)
			}
			if c.Name != "" {
				cmd.Args = append(cmd.Args, "-name", c.Name)
			}
			if c.Exclude != "" {
				cmd.Args = append(cmd.Args, "-exclude", c.Exclude)
			}
			if c.Active {
				cmd.Args = append(cmd.Args, "-active")
			}
			if c.GerritFetchMetaConfig {
				cmd.Args = append(cmd.Args, "-fetch-meta-config")
			}
			if c.GerritRepoNameFormat != "" {
				cmd.Args = append(cmd.Args, "-repo-name-format", c.GerritRepoNameFormat)
			}
			if !c.KeepDeleted {
				cmd.Args = append(cmd.Args, "-delete")
			}
			cmd.Args = append(cmd.Args, c.GerritApiURL)
		} else {
			log.Printf("executeMirror: ignoring config, because it does not contain any valid repository definition: %v", c)
			continue
		}

		stdout, _ := loggedRun(cmd)

		for _, fn := range bytes.Split(stdout, []byte{'\n'}) {
			if len(fn) == 0 {
				continue
			}

			pendingRepos <- string(fn)
		}

	}
}
