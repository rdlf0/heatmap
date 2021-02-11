package cmd

import (
	"context"
	"fmt"
	"log"

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
	Short: "Collects the diffs of the PRs that are not already analyzed",
	Long: `Gets all not already analyzed PRs and collects
their diff info which then writes into a MongoDB collection`,
	Run: collectDiffs,
}

var (
	jiraCollName   string
	githubCollName string
)

type diff struct {
	File      string `bson:"file"`
	Status    string `bson:"status"`
	Additions int    `bson:"additions"`
	Deletions int    `bson:"deletions"`
	Changes   int    `bson:"changes"`
}

type pr struct {
	ID   string `bson:"_id,omitempty"`
	Repo Repo   `bson:"repo"`
	PRID int    `bson:"pr_id"`
	Diff []diff `bson:"diff,omitempty"`
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

	jiraCollName = viper.GetString("mongo.collections.jira")
	githubCollName = viper.GetString("mongo.collections.github")
	jiraColl := mongoClient.Database(dbname).Collection(jiraCollName)
	prs := getNotAnalyzedPRs(ctx, jiraColl)
	fmt.Printf("New PRs found: %d\n", len(*prs))
	if len(*prs) == 0 {
		return
	}

	client := connectToGitHub(ctx)
	setPRsDiffs(ctx, client, prs)

	if len(*prs) == 0 {
		fmt.Println("No new PR changes")
	}

	docs := make([]interface{}, len(*prs))
	for i, v := range *prs {
		docs[i] = v
	}

	ghColl := mongoClient.Database(dbname).Collection(githubCollName)
	writeItemsToMongo(ctx, ghColl, docs)
}

func getNotAnalyzedPRs(ctx context.Context, collection *mongo.Collection) *[]pr {
	lookup := bson.D{{
		Key: "$lookup",
		Value: bson.M{
			"from":         githubCollName,
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

	prs := make([]pr, 0)
	for cur.Next(ctx) {
		p := &pr{}
		err := cur.Decode(&p)
		if err != nil {
			log.Fatal(err)
		}

		prs = append(prs, *p)
	}

	if err := cur.Err(); err != nil {
		log.Fatal(err)
	}

	return &prs
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

func setPRsDiffs(ctx context.Context, client *github.Client, prs *[]pr) {
	for k, p := range *prs {
		fmt.Printf("%+v\n", p)

		files, _, err := client.PullRequests.ListFiles(ctx, p.Repo.Owner, p.Repo.Name, p.PRID, &github.ListOptions{PerPage: 100})
		if err != nil {
			panic(err)
		}

		diffs := make([]diff, 0)
		for _, f := range files {
			fmt.Printf("File: %s\nadditions: %d; deletions: %d; changes: %d\n", *f.Filename, *f.Additions, *f.Deletions, *f.Changes)

			diff := &diff{
				File:      *f.Filename,
				Status:    *f.Status,
				Additions: *f.Additions,
				Deletions: *f.Deletions,
				Changes:   *f.Changes,
			}

			diffs = append(diffs, *diff)
		}

		(*prs)[k].Diff = diffs
	}
}
