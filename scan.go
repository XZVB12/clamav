package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/crackcomm/go-clitable"
	"github.com/fatih/structs"
	"github.com/maliceio/go-plugin-utils/database/elasticsearch"
	"github.com/maliceio/go-plugin-utils/utils"
	"github.com/parnurzeal/gorequest"
	"github.com/urfave/cli"
)

// Version stores the plugin's version
var Version string

// BuildTime stores the plugin's build time
var BuildTime string

const (
	name     = "clamav"
	category = "av"
)

type pluginResults struct {
	ID   string      `json:"id" structs:"id,omitempty"`
	Data ResultsData `json:"clamav" structs:"clamav"`
}

// ClamAV json object
type ClamAV struct {
	Results ResultsData `json:"clamav"`
}

// ResultsData json object
type ResultsData struct {
	Infected bool   `json:"infected" structs:"infected"`
	Result   string `json:"result" structs:"result"`
	Engine   string `json:"engine" structs:"engine"`
	Known    string `json:"known" structs:"known"`
	Updated  string `json:"updated" structs:"updated"`
	Error    string `json:"error" structs:"error"`
}

// ParseClamAvOutput convert clamav output into ClamAV struct
func ParseClamAvOutput(clamout string, err error) ResultsData {

	if err != nil {
		return ResultsData{Error: err.Error()}
	}

	clamAV := ResultsData{}

	lines := strings.Split(clamout, "\n")
	// Extract AV Scan Result
	result := lines[0]
	if len(result) != 0 {
		pathAndResult := strings.Split(result, ":")
		if strings.Contains(pathAndResult[1], "OK") {
			clamAV.Infected = false
		} else {
			clamAV.Infected = true
			clamAV.Result = strings.TrimSpace(strings.TrimRight(pathAndResult[1], "FOUND"))
		}
	} else {
		fmt.Println("[ERROR] empty scan result: ", result)
		os.Exit(2)
	}
	// Extract Clam Details from SCAN SUMMARY
	for _, line := range lines[1:] {
		if len(line) != 0 {
			keyvalue := strings.Split(line, ":")
			if len(keyvalue) != 0 {
				switch {
				case strings.Contains(keyvalue[0], "Known viruses"):
					clamAV.Known = strings.TrimSpace(keyvalue[1])
				case strings.Contains(line, "Engine version"):
					clamAV.Engine = strings.TrimSpace(keyvalue[1])
				}
			}
		}
	}

	clamAV.Updated = getUpdatedDate()

	return clamAV
}

func getUpdatedDate() string {
	if _, err := os.Stat("/opt/malice/UPDATED"); os.IsNotExist(err) {
		return BuildTime
	}
	updated, err := ioutil.ReadFile("/opt/malice/UPDATED")
	utils.Assert(err)
	return string(updated)
}

func printStatus(resp gorequest.Response, body string, errs []error) {
	fmt.Println(body)
}

func printMarkDownTable(clamav ClamAV) {
	fmt.Println("#### ClamAV")
	table := clitable.New([]string{"Infected", "Result", "Engine", "Updated"})
	table.AddRow(map[string]interface{}{
		"Infected": clamav.Results.Infected,
		"Result":   clamav.Results.Result,
		"Engine":   clamav.Results.Engine,
		// "Known":    clamav.Results.Known,
		"Updated": clamav.Results.Updated,
	})
	table.Markdown = true
	table.Print()
}

func updateAV(ctx context.Context) error {
	fmt.Println("Updating ClamAV...")
	fmt.Println(utils.RunCommand(ctx, "freshclam"))
	// Update UPDATED file
	t := time.Now().Format("20060102")
	err := ioutil.WriteFile("/opt/malice/UPDATED", []byte(t), 0644)
	return err
}

func main() {

	var elastic string

	cli.AppHelpTemplate = utils.AppHelpTemplate
	app := cli.NewApp()

	app.Name = "clamav"
	app.Author = "blacktop"
	app.Email = "https://github.com/blacktop"
	app.Version = Version + ", BuildTime: " + BuildTime
	app.Compiled, _ = time.Parse("20060102", BuildTime)
	app.Usage = "Malice ClamAV Plugin"
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "verbose, V",
			Usage: "verbose output",
		},
		cli.BoolFlag{
			Name:  "table, t",
			Usage: "output as Markdown table",
		},
		cli.BoolFlag{
			Name:   "post, p",
			Usage:  "POST results to Malice webhook",
			EnvVar: "MALICE_ENDPOINT",
		},
		cli.BoolFlag{
			Name:   "proxy, x",
			Usage:  "proxy settings for Malice webhook endpoint",
			EnvVar: "MALICE_PROXY",
		},
		cli.StringFlag{
			Name:        "elasitcsearch",
			Value:       "",
			Usage:       "elasitcsearch address for Malice to store results",
			EnvVar:      "MALICE_ELASTICSEARCH",
			Destination: &elastic,
		},
		cli.IntFlag{
			Name:   "timeout",
			Value:  60,
			Usage:  "malice plugin timeout (in seconds)",
			EnvVar: "MALICE_TIMEOUT",
		},
	}
	app.Commands = []cli.Command{
		{
			Name:    "update",
			Aliases: []string{"u"},
			Usage:   "Update virus definitions",
			Action: func(c *cli.Context) error {
				ctx, cancel := context.WithTimeout(
					context.Background(),
					time.Duration(c.Int("timeout"))*time.Second,
				)
				defer cancel()

				return updateAV(ctx)
			},
		},
	}
	app.Action = func(c *cli.Context) error {

		ctx, cancel := context.WithTimeout(
			context.Background(),
			time.Duration(c.Int("timeout"))*time.Second,
		)
		defer cancel()

		path := c.Args().First()

		if _, err := os.Stat(path); os.IsNotExist(err) {
			utils.Assert(err)
		}

		if c.Bool("verbose") {
			log.SetLevel(log.DebugLevel)
		}

		clamav := ClamAV{
			Results: ParseClamAvOutput(utils.RunCommand(ctx, "/usr/bin/clamscan", "--stdout", path)),
		}

		// upsert into Database
		elasticsearch.InitElasticSearch(elastic)
		elasticsearch.WritePluginResultsToDatabase(elasticsearch.PluginResults{
			ID:       utils.Getopt("MALICE_SCANID", utils.GetSHA256(path)),
			Name:     name,
			Category: category,
			Data:     structs.Map(clamav.Results),
		})

		if c.Bool("table") {
			printMarkDownTable(clamav)
		} else {
			fprotJSON, err := json.Marshal(clamav)
			utils.Assert(err)
			if c.Bool("post") {
				request := gorequest.New()
				if c.Bool("proxy") {
					request = gorequest.New().Proxy(os.Getenv("MALICE_PROXY"))
				}
				request.Post(os.Getenv("MALICE_ENDPOINT")).
					Set("X-Malice-ID", utils.Getopt("MALICE_SCANID", utils.GetSHA256(path))).
					Send(string(fprotJSON)).
					End(printStatus)

				return nil
			}
			fmt.Println(string(fprotJSON))
		}
		return nil
	}

	err := app.Run(os.Args)
	utils.Assert(err)
}
