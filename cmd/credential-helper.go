// Copyright 2018 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package cmd

import (
	"fmt"

	"github.com/urfave/cli"
)

// CmdMigrate represents the available migrate sub-command.
var (
	CmdCredentialHelper = cli.Command{
		Name:        "credential-helper",
		Usage:       "Git credential helper which echo username and password",
		Description: "This is a used by git command.",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "username",
				Usage: `Username`,
				Value: "",
			},
			cli.StringFlag{
				Name:  "password",
				Usage: "Password",
				Value: "",
			},
		},
		Action: runGetCredentials,
	}
)

func runGetCredentials(c *cli.Context) error {
	username := c.String("username")
	password := c.String("password")

	if len(username) > 0 || len(password) > 0 {
		if len(username) > 0 {
			fmt.Printf("username=%s\n", username)
		}
		if len(password) > 0 {
			fmt.Printf("password=%s\n", password)
		}
	}
	return nil
}
