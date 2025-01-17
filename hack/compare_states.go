/*
Copyright 2021 The TestGrid Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// A script to quickly check two TestGrid state.protos do not wildly differ.
// Assume that if the column and row names are approx. equivalent, the state
// is probably reasonable.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/GoogleCloudPlatform/testgrid/util/gcs"
	"github.com/golang/protobuf/proto"
	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
	"os"
	"path/filepath"
	"strings"

	statepb "github.com/GoogleCloudPlatform/testgrid/pb/state"
)

type options struct {
	first, second gcs.Path
	creds         string
}

// validate ensures reasonable options
func (o *options) validate() error {
	if o.first.String() == "" || o.second.String() == "" {
		return errors.New("--first and --second are required and must specify directories.")
	}
	if !strings.HasSuffix(o.first.String(), "/") {
		o.first.Set(o.first.String() + "/")
	}
	if !strings.HasSuffix(o.second.String(), "/") {
		o.second.Set(o.second.String() + "/")
	}

	return nil
}

// gatherOptions reads options from flags
func gatherFlagOptions(fs *flag.FlagSet, args ...string) options {
	var o options
	fs.Var(&o.first, "first", "First directory of state files to compare.")
	fs.Var(&o.second, "second", "Second directory of state files to compare.")
	fs.StringVar(&o.creds, "gcp-service-account", "", "/path/to/gcp/creds (use local creds if empty)")
	fs.Parse(args)
	return o
}

// gatherOptions reads options from flags
func gatherOptions() options {
	return gatherFlagOptions(flag.CommandLine, os.Args[1:]...)
}

func rowsAndColumns(ctx context.Context, grid *statepb.Grid) (map[string]bool, map[string]bool) {
	rows := make(map[string]bool)
	for _, row := range grid.GetRows() {
		rows[row.GetName()] = true
	}

	columns := make(map[string]bool)
	for _, column := range grid.GetColumns() {
		columns[proto.MarshalTextString(column)] = true
	}

	return rows, columns
}

func compare(ctx context.Context, first, second *statepb.Grid) bool {
	var diffed bool
	logrus.Infof("*****************************")
	logrus.Infof("Comparing %q and %q...", first.GetConfig().GetName(), second.GetConfig().GetName())
	firstRows, firstColumns := rowsAndColumns(ctx, first)
	secondRows, secondColumns := rowsAndColumns(ctx, second)

	logrus.Infof("Checking row names...")
	if diff := cmp.Diff(firstRows, secondRows); diff != "" {
		logrus.Infof("\tRows differ (-first, +second): %s", diff)
		diffed = true
	} else {
		logrus.Infof("\tAll %d rows match!", len(firstRows))
	}

	logrus.Infof("Checking column data...")
	if diff := cmp.Diff(firstColumns, secondColumns); diff != "" {
		logrus.Infof("Columns differ (-first, +second): %s", diff)
		diffed = true
	} else {
		logrus.Infof("\tAll %d columns match!", len(firstColumns))
	}
	return diffed
}

func filenames(ctx context.Context, dir gcs.Path, client gcs.Client) ([]string, error) {
	stats := client.Objects(ctx, dir, "/", "")
	var filenames []string
	for {
		stat, err := stats.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		filename := filepath.Join(dir.String(), filepath.Base(stat.Name))
		filenames = append(filenames, filename)
	}
	return filenames, nil
}

func main() {
	opt := gatherOptions()
	if err := opt.validate(); err != nil {
		logrus.Fatalf("Invalid options %v: %v", opt, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storageClient, err := gcs.ClientWithCreds(ctx, opt.creds)
	if err != nil {
		logrus.Fatalf("Failed to create storage client: %v", err)
	}
	defer storageClient.Close()

	client := gcs.NewClient(storageClient)

	firstFiles, err := filenames(ctx, opt.first, client)
	if err != nil {
		logrus.Fatalf("Failed to list files in %q: %v", opt.first.String(), err)
	}
	var diffedMsgs, errorMsgs []string
	for _, firstP := range firstFiles {
		secondP := filepath.Join(opt.second.String(), filepath.Base(firstP))
		firstPath, err := gcs.NewPath(firstP)
		if err != nil {
			errorMsgs = append(errorMsgs, fmt.Sprintf("gcs.NewPath(%q): %v", firstP, err))
			continue
		}
		firstGrid, err := gcs.DownloadGrid(ctx, client, *firstPath)
		if err != nil {
			errorMsgs = append(errorMsgs, fmt.Sprintf("gcs.DownloadGrid(%q): %v", firstP, err))
			continue
		}
		secondPath, err := gcs.NewPath(secondP)
		if err != nil {
			errorMsgs = append(errorMsgs, fmt.Sprintf("gcs.NewPath(%q): %v", secondP, err))
			continue
		}
		secondGrid, err := gcs.DownloadGrid(ctx, client, *secondPath)
		if err != nil {
			errorMsgs = append(errorMsgs, fmt.Sprintf("gcs.DownloadGrid(%q): %v", secondP, err))
			continue
		}
		if diffed := compare(ctx, firstGrid, secondGrid); diffed {
			diffedMsgs = append(diffedMsgs, fmt.Sprintf("%q vs. %q", firstP, secondP))
		}
	}
	logrus.Infof("Found diffs for %d pairs:", len(diffedMsgs))
	for _, msg := range diffedMsgs {
		logrus.Infof("\t*%s", msg)
	}
	if n := len(errorMsgs); n > 0 {
		logrus.WithField("count", n).WithField("errors", errorMsgs).Fatal("Errors when diffing directories.")
	}
}
