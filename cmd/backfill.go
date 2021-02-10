package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// backfillCmd represents the backfill command
var backfillCmd = &cobra.Command{
	Use:   "backfill",
	Short: "Generates the mappings of Jira Issues and GitHub PRs",
	Long: `Finds all current bugs in the specified Jira project
and their corresponding GitHub PRs. After that writes these
mappings into a MongoDB collection.`,
	Run: backfill,
}

var (
	client      = &http.Client{}
	jiraHost    string
	jiraProject string
	dbname      string
)

// Bug represents a separate jira issue/bug
type Bug struct {
	ID  int    `json:"id,string"`
	Key string `json:"key"`
}

// IssuesResponse represents a response with issues
type IssuesResponse struct {
	StartAt    int   `json:"startAt"`
	MaxResults int   `json:"maxResults"`
	Total      int   `json:"total"`
	Issues     []Bug `json:"issues"`
}

// JiraPR is a representation of a PR data in Jira
type JiraPR struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	URL    string `json:"url"`
}

// DevStatusResponse represents a response of an issue's dev status
type DevStatusResponse struct {
	Detail []struct {
		PRs []JiraPR `json:"pullRequests"`
	} `json:"detail"`
}

// MongoMapping represents a mapping of a Jira Isuse and a GitHub PR
type MongoMapping struct {
	ID      string `bson:"_id,omitempty"`
	Project string `bson:"project"`
	IssueID int    `bson:"issue_id"`
	Repo    string `bson:"repo"`
	PRID    int    `bson:"pr_id"`
}

func init() {
	rootCmd.AddCommand(backfillCmd)
}

func backfill(cmd *cobra.Command, args []string) {
	jiraHost = viper.GetString("jira.host")
	jiraProject = viper.GetString("jira.project")
	jiraEmail := viper.GetString("jira.auth.email")
	jiraToken := viper.GetString("jira.auth.token")
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", jiraEmail, jiraToken)))

	bugs := collectBugs(auth)

	ctx, cancel, mongoClient := connectToMongo()
	defer cancel()
	defer func() {
		if err := mongoClient.Disconnect(ctx); err != nil {
			panic(err)
		}
	}()
	coll := mongoClient.Database(dbname).Collection("jira")

	alreadyMapped := getAlreadyMappedIssueIDs(ctx, coll)
	newMappingsByIssueID := make(map[int]*[]JiraPR)
	for _, b := range *bugs {
		if _, ok := alreadyMapped[b.ID]; !ok {
			if ds, err := findDevStatus(b, auth); err == nil {
				newMappingsByIssueID[b.ID] = ds
			}
		}
	}

	if len(newMappingsByIssueID) == 0 {
		fmt.Println("No new mappings found")
		return
	}

	newMappings := convertJiraMappingsToMongoMappings(newMappingsByIssueID)
	if len(*newMappings) == 0 {
		fmt.Println("No new merged PRs found")
		return
	}

	docs := make([]interface{}, len(*newMappings))
	for i, v := range *newMappings {
		docs[i] = v
	}

	writeItemsToMongo(ctx, coll, docs)
}

func collectBugs(auth string) *[]Bug {
	queryParams := url.Values{}
	queryParams.Add("jql", fmt.Sprintf("project = %s and type = Bug", jiraProject))
	queryParams.Add("fields", "id,key")
	queryParams.Add("maxResults", "5")

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/rest/api/latest/search?%s", jiraHost, queryParams.Encode()), nil)
	if err != nil {
		panic(err)
	}
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", auth))
	req.Header.Add("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)

	bugs := &IssuesResponse{}
	err = decoder.Decode(bugs)
	if err != nil {
		panic(err)
	}

	fmt.Printf("%+v\n", bugs)

	return &bugs.Issues
}

func connectToMongo() (context.Context, context.CancelFunc, *mongo.Client) {
	srv := viper.GetString("mongo.srv")
	user := viper.GetString("mongo.user")
	pass := viper.GetString("mongo.password")
	dbname = viper.GetString("mongo.dbname")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(
		fmt.Sprintf(srv, user, pass, dbname),
	))
	if err != nil {
		log.Fatal(err)
	}

	return ctx, cancel, client
}

func getAlreadyMappedIssueIDs(ctx context.Context, collection *mongo.Collection) map[int]bool {
	projection := options.Find().SetProjection(bson.M{"_id": 0, "issue_id": 1})

	cur, err := collection.Find(ctx, bson.D{}, projection)
	if err != nil {
		log.Fatal(err)
	}
	defer cur.Close(ctx)

	mappings := make(map[int]bool, 0)
	for cur.Next(ctx) {
		result := &MongoMapping{}
		err := cur.Decode(&result)
		if err != nil {
			log.Fatal(err)
		}

		mappings[result.IssueID] = false
	}

	if err := cur.Err(); err != nil {
		log.Fatal(err)
	}

	return mappings
}

func findDevStatus(b Bug, auth string) (*[]JiraPR, error) {
	queryParams := url.Values{}
	queryParams.Add("issueId", strconv.Itoa(b.ID))
	queryParams.Add("applicationType", "GitHub")
	queryParams.Add("dataType", "pullrequest")

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/rest/dev-status/latest/issue/detail?%s", jiraHost, queryParams.Encode()), nil)
	if err != nil {
		panic(err)
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", auth))
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)

	devStatus := &DevStatusResponse{}
	err = decoder.Decode(devStatus)
	if err != nil {
		panic(err)
	}

	if len(devStatus.Detail[0].PRs) == 0 {
		return nil, errors.New("Dev status not found")
	}

	return &devStatus.Detail[0].PRs, nil
}

func convertJiraMappingsToMongoMappings(jiraMappings map[int]*[]JiraPR) *[]MongoMapping {
	result := make([]MongoMapping, 0)

	for k, v := range jiraMappings {
		for _, pr := range *v {
			if pr.Status != "MERGED" {
				continue
			}

			repoURL := strings.Split(pr.URL, "/pull")[0]
			repo := strings.Split(repoURL, "github.com/")[1]

			var m MongoMapping
			m.Project = jiraProject
			m.IssueID = k
			m.Repo = repo
			m.PRID, _ = strconv.Atoi(pr.ID[1:])

			result = append(result, m)
		}
	}

	return &result
}

func writeItemsToMongo(ctx context.Context, coll *mongo.Collection, docs []interface{}) {
	res, err := coll.InsertMany(ctx, docs, nil)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Inserted IDs (%d): %s\n", len(res.InsertedIDs), res.InsertedIDs)
}
