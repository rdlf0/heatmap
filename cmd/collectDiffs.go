package cmd

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/google/go-github/github"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/oauth2"
)

// collectDiffsCmd represents the collectDiffs command
var collectDiffsCmd = &cobra.Command{
	Use:   "collectDiffs",
	Short: "A brief description of your command",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	Run: collectDiffs,
}

// PR represents a pair of repo name and PR ID
type PR struct {
	Repo string `bson:"repo"`
	PRID int    `bson:"pr_id"`
}

type diff struct {
	Additions int `bson:"additions"`
	Deletions int `bson:"deletions"`
	Changes   int `bson:"changes"`
}

type change struct {
	File   string `bson:"file"`
	Status string `bson:"status"`
	Diff   diff   `bson:"diff"`
}

type prChanges struct {
	ID      string   `bson:"_id,omitempty"`
	Repo    string   `bson:"repo"`
	PRID    int      `bson:"pr_id"`
	Changes []change `bson:"changes"`
}

func init() {
	rootCmd.AddCommand(collectDiffsCmd)
}

func collectDiffs(cmd *cobra.Command, args []string) {
	ctx, cancel, mongoClient := connectToMongo()
	defer cancel()
	defer func() {
		if err := mongoClient.Disconnect(ctx); err != nil {
			panic(err)
		}
	}()
	jiraColl := mongoClient.Database(dbname).Collection("jira")
	prs := getNotAnalyzedPRs(ctx, jiraColl)
	fmt.Printf("New PRs found: %d\n", len(*prs))
	if len(*prs) == 0 {
		return
	}

	client := connectToGitHub(ctx)
	prChs := collectPRChanges(ctx, client, prs)

	if len(*prChs) == 0 {
		fmt.Println("No new PR changes")
	}

	docs := make([]interface{}, len(*prChs))
	for i, v := range *prChs {
		docs[i] = v
	}

	ghColl := mongoClient.Database(dbname).Collection("github")
	writeItemsToMongo(ctx, ghColl, docs)
}

func getNotAnalyzedPRs(ctx context.Context, collection *mongo.Collection) *[]PR {
	lookup := bson.D{{
		Key: "$lookup",
		Value: bson.M{
			"from":         "github",
			"localField":   "pr_id",
			"foreignField": "pr_id",
			"as":           "pr",
		},
	}}

	match := bson.D{{
		Key: "$match",
		Value: bson.M{
			"pr": bson.M{
				"$size": 0,
			},
		},
	}}

	project := bson.D{{
		Key: "$project",
		Value: bson.M{
			"_id":   0,
			"repo":  1,
			"pr_id": 1,
		},
	}}

	cur, err := collection.Aggregate(ctx, mongo.Pipeline{lookup, match, project})
	if err != nil {
		log.Fatal(err)
	}
	defer cur.Close(ctx)

	PRs := make([]PR, 0)
	for cur.Next(ctx) {
		p := &PR{}
		err := cur.Decode(&p)
		if err != nil {
			log.Fatal(err)
		}

		PRs = append(PRs, *p)
	}

	if err := cur.Err(); err != nil {
		log.Fatal(err)
	}

	return &PRs
}

func connectToGitHub(ctx context.Context) *github.Client {
	token := viper.GetString("github.token")
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	return client
}

func collectPRChanges(ctx context.Context, client *github.Client, PRs *[]PR) *[]prChanges {
	prChs := make([]prChanges, 0)
	for _, p := range *PRs {
		fmt.Printf("%+v\n", p)

		repoParts := strings.Split(p.Repo, "/")
		files, _, err := client.PullRequests.ListFiles(ctx, repoParts[0], repoParts[1], p.PRID, &github.ListOptions{PerPage: 100})
		if err != nil {
			panic(err)
		}

		changes := make([]change, 0)
		for _, f := range files {
			fmt.Printf("File: %s\nadditions: %d\ndeletions: %d\nchanges: %d\n", *f.Filename, *f.Additions, *f.Deletions, *f.Changes)
			d := &diff{
				Additions: *f.Additions,
				Deletions: *f.Deletions,
				Changes:   *f.Changes,
			}

			ch := &change{
				File:   *f.Filename,
				Status: *f.Status,
				Diff:   *d,
			}

			changes = append(changes, *ch)
		}

		prCh := &prChanges{
			Repo:    p.Repo,
			PRID:    p.PRID,
			Changes: changes,
		}

		prChs = append(prChs, *prCh)
	}

	return &prChs
}
