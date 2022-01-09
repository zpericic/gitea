// Copyright 2021 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package migration

import (
	"context"
	"net/url"
)

// NullDownloader implements a blank downloader
type NullDownloader struct {
}

var (
	_ Downloader = &NullDownloader{}
)

// SetContext set context
func (n NullDownloader) SetContext(_ context.Context) {}

// GetRepoInfo returns a repository information
func (n NullDownloader) GetRepoInfo() (*Repository, error) {
	return nil, &ErrNotSupported{Entity: "RepoInfo"}
}

// GetTopics return repository topics
func (n NullDownloader) GetTopics() ([]string, error) {
	return nil, &ErrNotSupported{Entity: "Topics"}
}

// GetMilestones returns milestones
func (n NullDownloader) GetMilestones() ([]*Milestone, error) {
	return nil, &ErrNotSupported{Entity: "Milestones"}
}

// GetReleases returns releases
func (n NullDownloader) GetReleases() ([]*Release, error) {
	return nil, &ErrNotSupported{Entity: "Releases"}
}

// GetLabels returns labels
func (n NullDownloader) GetLabels() ([]*Label, error) {
	return nil, &ErrNotSupported{Entity: "Labels"}
}

// GetIssues returns issues according start and limit
func (n NullDownloader) GetIssues(page, perPage int) ([]*Issue, bool, error) {
	return nil, false, &ErrNotSupported{Entity: "Issues"}
}

// GetComments returns comments according the options
func (n NullDownloader) GetComments(GetCommentOptions) ([]*Comment, bool, error) {
	return nil, false, &ErrNotSupported{Entity: "Comments"}
}

// GetPullRequests returns pull requests according page and perPage
func (n NullDownloader) GetPullRequests(page, perPage int) ([]*PullRequest, bool, error) {
	return nil, false, &ErrNotSupported{Entity: "PullRequests"}
}

// GetReviews returns pull requests review
func (n NullDownloader) GetReviews(pullRequestContext IssueContext) ([]*Review, error) {
	return nil, &ErrNotSupported{Entity: "Reviews"}
}

// FormatCloneURL add authentication into remote URLs
func (n NullDownloader) FormatCloneURL(opts MigrateOptions, remoteAddr string) (string, string, string, error) {
	u, err := url.Parse(remoteAddr)
	if err != nil {
		return "", "", "", err
	}
	if len(opts.AuthToken) > 0 {
		u.User = nil
		return u.String(), "oauth2", opts.AuthToken, nil
	}
	var username string
	var password string
	if len(opts.AuthUsername) > 0 {
		username = opts.AuthUsername
		password = opts.AuthPassword
		if len(u.User.Username()) > 0 {
			u.User = url.User(u.User.Username())
		} else {
			u.User = nil
		}
	} else {
		username = u.User.Username()
		urlPassword, found := u.User.Password()
		if found {
			password = urlPassword
		}
		u.User = url.User(u.User.Username())
	}
	return u.String(), username, password, nil
}

// SupportGetRepoComments return true if it supports get repo comments
func (n NullDownloader) SupportGetRepoComments() bool {
	return false
}
