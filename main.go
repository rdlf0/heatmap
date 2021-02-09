package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/viper"
)

// IssuesResponse represents a response with issues
type IssuesResponse struct {
	StartAt    int `json:"startAt"`
	MaxResults int `json:"maxResults"`
	Total      int `json:"total"`
	Issues     []struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	} `json:"issues"`
}

// DevStatusResponse represents a response of an issue's dev status
type DevStatusResponse struct {
	Detail []struct {
		PRs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			URL    string `json:"url"`
		} `json:"pullRequests"`
	} `json:"detail"`
}

func init() {
	viper.SetConfigName("config")
	viper.SetConfigType("json")
	viper.AddConfigPath(".")
	viper.AutomaticEnv()
	err := viper.ReadInConfig()
	if err != nil {
		panic("Cannot read config")
	}
}

func main() {
	jiraHost := viper.GetString("jira.host")
	jiraProject := viper.GetString("jira.project")

	queryParams := url.Values{}
	queryParams.Add("jql", fmt.Sprintf("project = %s and type = Bug", jiraProject))
	queryParams.Add("fields", "id,key")

	client := &http.Client{}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/rest/api/latest/search?%s", jiraHost, queryParams.Encode()), nil)
	if err != nil {
		panic(err)
	}

	jiraEmail := viper.GetString("jira.auth.email")
	jiraToken := viper.GetString("jira.auth.token")
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", jiraEmail, jiraToken)))
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

	for _, b := range bugs.Issues {
		queryParams = url.Values{}
		queryParams.Add("issueId", b.ID)
		queryParams.Add("applicationType", "GitHub")
		queryParams.Add("dataType", "pullrequest")
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/rest/dev-status/latest/issue/detail?%s", jiraHost, queryParams.Encode()), nil)
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

		fmt.Printf("%s -> %+v\n", b.Key, devStatus)
	}
}
