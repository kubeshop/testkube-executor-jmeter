package runner

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kubeshop/testkube-executor-jmeter/pkg/parser"
	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/envs"
	"github.com/kubeshop/testkube/pkg/executor"
	"github.com/kubeshop/testkube/pkg/executor/content"
	"github.com/kubeshop/testkube/pkg/executor/output"
	"github.com/kubeshop/testkube/pkg/executor/runner"
	"github.com/kubeshop/testkube/pkg/executor/scraper"
	"github.com/kubeshop/testkube/pkg/executor/secret"
	"github.com/kubeshop/testkube/pkg/ui"
)

func NewRunner() (*JMeterRunner, error) {
	output.PrintLog(fmt.Sprintf("%s Preparing test runner", ui.IconTruck))
	params, err := envs.LoadTestkubeVariables()
	if err != nil {
		return nil, fmt.Errorf("could not initialize JMeter runner variables: %w", err)
	}

	return &JMeterRunner{
		Params:  params,
		Fetcher: content.NewFetcher(""),
		Scraper: scraper.NewMinioScraper(
			params.Endpoint,
			params.AccessKeyID,
			params.SecretAccessKey,
			params.Location,
			params.Token,
			params.Bucket,
			params.Ssl,
		),
	}, nil
}

// JMeterRunner runner
type JMeterRunner struct {
	Params  envs.Params
	Fetcher content.ContentFetcher
	Scraper scraper.Scraper
}

func (r *JMeterRunner) Run(execution testkube.Execution) (result testkube.ExecutionResult, err error) {

	output.PrintEvent(
		fmt.Sprintf("%s Running with config", ui.IconTruck),
		"scraperEnabled", r.Params.ScrapperEnabled,
		"dataDir", r.Params.DataDir,
		"SSL", r.Params.Ssl,
		"endpoint", r.Params.Endpoint,
	)

	envManager := secret.NewEnvManagerWithVars(execution.Variables)
	envManager.GetVars(envManager.Variables)

	gitUsername := r.Params.GitUsername
	gitToken := r.Params.GitToken
	if gitUsername != "" || gitToken != "" {
		if execution.Content != nil && execution.Content.Repository != nil {
			execution.Content.Repository.Username = gitUsername
			execution.Content.Repository.Token = gitToken
		}
	}

	path, err := r.Fetcher.Fetch(execution.Content)
	if err != nil {
		return result, err
	}

	// Only file based tests in first iteration
	if execution.Content.IsDir() || !execution.Content.IsFile() {
		output.PrintLog(fmt.Sprintf("%s Unsupported content type, use file based content", ui.IconCross))
		return result, fmt.Errorf("unsupported content type, use file based content")
	}

	// compose parameters passed to JMeter with -J
	params := make([]string, 0, len(envManager.Variables))
	for _, value := range envManager.Variables {
		params = append(params, fmt.Sprintf("-J%s=%s", value.Name, value.Value))
	}

	runPath := r.Params.DataDir
	if execution.Content.Repository != nil && execution.Content.Repository.WorkingDir != "" {
		runPath = filepath.Join(r.Params.DataDir, "repo", execution.Content.Repository.WorkingDir)
	}

	reportPath := filepath.Join(runPath, "report.jtl")
	args := []string{"-n", "-t", path, "-l", reportPath}
	args = append(args, params...)

	// append args from execution
	args = append(args, execution.Args...)
	output.PrintLog(fmt.Sprintf("%s Using arguments: %v", ui.IconWorld, args))

	// run JMeter inside repo directory ignore execution error in case of failed test
	out, err := executor.Run(runPath, "jmeter", envManager, args...)
	if err != nil {
		return *result.WithErrors(fmt.Errorf("jmeter run error: %w", err)), nil
	}
	out = envManager.Obfuscate(out)

	output.PrintLog(fmt.Sprintf("%s Getting report %s", ui.IconFile, reportPath))
	f, err := os.Open(reportPath)
	if err != nil {
		return *result.WithErrors(fmt.Errorf("getting jtl report error: %w", err)), nil
	}

	results := parser.Parse(f)
	executionResult := MapResultsToExecutionResults(out, results)
	output.PrintLog(fmt.Sprintf("%s Mapped JMeter results to Execution Results...", ui.IconCheckMark))

	// scrape artifacts first even if there are errors above
	// Basic implementation will scrape report
	// TODO add additional artifacts to scrape
	if r.Params.ScrapperEnabled {
		directories := []string{
			reportPath,
		}

		err := r.Scraper.Scrape(execution.Id, directories)
		if err != nil {
			return *executionResult.WithErrors(fmt.Errorf("scrape artifacts error: %w", err)), nil
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
				AssertionResults: []testkube.AssertionResult{{
					Name:   r.Label,
					Status: MapStatus(r),
				}},
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

// GetType returns runner type
func (r *JMeterRunner) GetType() runner.Type {
	return runner.TypeMain
}
