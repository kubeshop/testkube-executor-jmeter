package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kelseyhightower/envconfig"
	"github.com/kubeshop/testkube-executor-jmeter/pkg/parser"
	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/executor"
	"github.com/kubeshop/testkube/pkg/executor/content"
	"github.com/kubeshop/testkube/pkg/executor/output"
	"github.com/kubeshop/testkube/pkg/executor/scraper"
	"github.com/kubeshop/testkube/pkg/executor/secret"
)

type Params struct {
	Endpoint        string // RUNNER_ENDPOINT
	AccessKeyID     string // RUNNER_ACCESSKEYID
	SecretAccessKey string // RUNNER_SECRETACCESSKEY
	Location        string // RUNNER_LOCATION
	Token           string // RUNNER_TOKEN
	Ssl             bool   // RUNNER_SSL
	ScrapperEnabled bool   // RUNNER_SCRAPPERENABLED
	GitUsername     string // RUNNER_GITUSERNAME
	GitToken        string // RUNNER_GITTOKEN
	Datadir         string // RUNNER_DATADIR
}

func NewRunner() (*JMeterRunner, error) {
	var params Params
	err := envconfig.Process("runner", &params)
	if err != nil {
		return nil, err
	}

	return &JMeterRunner{
		Fetcher: content.NewFetcher(""),
		Scraper: scraper.NewMinioScraper(
			params.Endpoint,
			params.AccessKeyID,
			params.SecretAccessKey,
			params.Location,
			params.Token,
			params.Ssl,
		),
	}, nil
}

// ExampleRunner for jmeter - change me to some valid runner
type JMeterRunner struct {
	Params  Params
	Fetcher content.ContentFetcher
	Scraper scraper.Scraper
}

func (r *JMeterRunner) Run(execution testkube.Execution) (result testkube.ExecutionResult, err error) {

	secret.NewEnvManager().GetVars(execution.Variables)
	path, err := r.Fetcher.Fetch(execution.Content)
	if err != nil {
		return result, err
	}

	output.PrintEvent("created content path", path)

	// Only file based tests in first iteration
	if execution.Content.IsDir() || !execution.Content.IsFile() {
		return result, fmt.Errorf("unsupported content type use file based content")
	}

	// compose parameters passed to JMeter with -J
	params := make([]string, 0, len(execution.Variables))
	for _, value := range execution.Variables {
		params = append(params, fmt.Sprintf(" -J%s=%s", value.Name, value.Value))
	}

	runPath := r.Params.Datadir
	reportPath := filepath.Join(runPath, "report.jtl")
	args := []string{"-n", "-t", path, "-l", reportPath, strings.Join(params, " ")}

	// append args from execution
	args = append(args, execution.Args...)

	envManager := secret.NewEnvManagerWithVars(execution.Variables)
	envManager.GetVars(execution.Variables)

	// run JMeter inside repo directory ignore execution error in case of failed test
	out, err := executor.Run(runPath, "jmeter", envManager, args...)
	if err != nil {
		return result.WithErrors(fmt.Errorf("jmeter run error: %w", err)), nil
	}
	out = envManager.Obfuscate(out)

	f, err := os.Open(reportPath)
	if err != nil {
		return result.WithErrors(fmt.Errorf("getting jtl report error: %w", err)), nil
	}

	results := parser.Parse(f)
	executionResult := MapResultsToExecutionResults(out, results)

	// scrape artifacts first even if there are errors above
	// Basic implementation will scrape report
	// TODO add additional artifacts to scrape
	if r.Params.ScrapperEnabled {
		directories := []string{
			reportPath,
		}
		err := r.Scraper.Scrape(execution.Id, directories)
		if err != nil {
			return executionResult.WithErrors(fmt.Errorf("scrape artifacts error: %w", err)), nil
		}
	}

	return executionResult, nil
}

func MapResultsToExecutionResults(out []byte, results parser.Results) (result testkube.ExecutionResult) {
	result.Status = testkube.ExecutionStatusPassed
	if results.HasError {
		result.Status = testkube.ExecutionStatusFailed
		result.ErrorMessage = results.LastErrorMessage
	}

	result.Output = string(out)
	result.OutputType = "text/plain"

	for _, r := range results.Results {
		result.Steps = append(
			result.Steps,
			testkube.ExecutionStepResult{
				Name:     r.Label,
				Duration: r.Duration.String(),
				Status:   MapStatus(r),
			})
	}

	return result
}

func MapStatus(result parser.Result) string {
	if result.Success {
		return string(testkube.PASSED_ExecutionStatus)
	}

	return string(testkube.FAILED_ExecutionStatus)
}
