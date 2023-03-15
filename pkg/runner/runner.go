package runner

import (
	"context"
	"fmt"
	cloudagent "github.com/kubeshop/testkube/pkg/agent"
	"github.com/kubeshop/testkube/pkg/cloud"
	cloudscraper "github.com/kubeshop/testkube/pkg/cloud/data/artifact"
	cloudexecutor "github.com/kubeshop/testkube/pkg/cloud/data/executor"
	"github.com/kubeshop/testkube/pkg/executor/scraper"
	"github.com/kubeshop/testkube/pkg/filesystem"
	"github.com/kubeshop/testkube/pkg/log"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"os"
	"path/filepath"

	"github.com/kubeshop/testkube-executor-jmeter/pkg/parser"
	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/envs"
	"github.com/kubeshop/testkube/pkg/executor"
	"github.com/kubeshop/testkube/pkg/executor/env"
	"github.com/kubeshop/testkube/pkg/executor/output"
	"github.com/kubeshop/testkube/pkg/executor/runner"
	"github.com/kubeshop/testkube/pkg/ui"
)

func NewRunner() (*JMeterRunner, error) {
	output.PrintLog(fmt.Sprintf("%s Preparing test runner", ui.IconTruck))
	params, err := envs.LoadTestkubeVariables()
	if err != nil {
		return nil, fmt.Errorf("could not initialize JMeter runner variables: %w", err)
	}

	r := &JMeterRunner{
		Params: params,
	}

	if params.CloudMode {
		grpcConn, err := cloudagent.NewGRPCConnection(
			context.Background(),
			params.CloudAPITLSInsecure,
			params.CloudAPIURL,
			log.DefaultLogger,
		)
		if err != nil {
			return nil, errors.Errorf("error establishing gRPC connection with cloud API: %v", err)
		}
		grpcClient := cloud.NewTestKubeCloudAPIClient(grpcConn)
		r.GRPCConn = grpcConn
		r.CloudClient = grpcClient
	}

	return r, nil
}

// JMeterRunner runner
type JMeterRunner struct {
	Params      envs.Params
	GRPCConn    *grpc.ClientConn
	CloudClient cloud.TestKubeCloudAPIClient
}

func (r *JMeterRunner) Run(execution testkube.Execution) (result testkube.ExecutionResult, err error) {

	output.PrintEvent(
		fmt.Sprintf("%s Running with config", ui.IconTruck),
		"scraperEnabled", r.Params.ScrapperEnabled,
		"dataDir", r.Params.DataDir,
		"SSL", r.Params.Ssl,
		"endpoint", r.Params.Endpoint,
	)

	envManager := env.NewManagerWithVars(execution.Variables)
	envManager.GetReferenceVars(envManager.Variables)

	gitUsername := r.Params.GitUsername
	gitToken := r.Params.GitToken
	if gitUsername != "" || gitToken != "" {
		if execution.Content != nil && execution.Content.Repository != nil {
			execution.Content.Repository.Username = gitUsername
			execution.Content.Repository.Token = gitToken
		}
	}

	path := ""
	workingDir := ""
	basePath, _ := filepath.Abs(r.Params.DataDir)
	if execution.Content != nil {
		isStringContentType := execution.Content.Type_ == string(testkube.TestContentTypeString)
		isFileURIContentType := execution.Content.Type_ == string(testkube.TestContentTypeFileURI)
		if isStringContentType || isFileURIContentType {
			path = filepath.Join(basePath, "test-content")
		}

		isGitFileContentType := execution.Content.Type_ == string(testkube.TestContentTypeGitFile)
		isGitDirContentType := execution.Content.Type_ == string(testkube.TestContentTypeGitDir)
		isGitContentType := execution.Content.Type_ == string(testkube.TestContentTypeGit)
		if isGitFileContentType || isGitDirContentType || isGitContentType {
			path = filepath.Join(basePath, "repo")
			if execution.Content.Repository != nil {
				path = filepath.Join(path, execution.Content.Repository.Path)
				workingDir = execution.Content.Repository.WorkingDir
			}
		}
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		return result, err
	}

	if fileInfo.IsDir() {
		scriptName := execution.Args[len(execution.Args)-1]
		if workingDir != "" {
			path = filepath.Join(r.Params.DataDir, "repo")
			if execution.Content != nil && execution.Content.Repository != nil {
				scriptName = filepath.Join(execution.Content.Repository.Path, scriptName)
			}
		}

		execution.Args = execution.Args[:len(execution.Args)-1]
		output.PrintLog(fmt.Sprintf("%s It is a directory test - trying to find file from the last executor argument %s in directory %s", ui.IconWorld, scriptName, path))

		// sanity checking for test script
		scriptFile := filepath.Join(path, workingDir, scriptName)
		fileInfo, errFile := os.Stat(scriptFile)
		if errors.Is(errFile, os.ErrNotExist) || fileInfo.IsDir() {
			output.PrintLog(fmt.Sprintf("%s Could not find file %s in the directory, error: %s", ui.IconCross, scriptName, errFile))
			return *result.Err(fmt.Errorf("could not find file %s in the directory: %w", scriptName, errFile)), nil
		}
		path = scriptFile
	}

	// compose parameters passed to JMeter with -J
	params := make([]string, 0, len(envManager.Variables))
	for _, value := range envManager.Variables {
		params = append(params, fmt.Sprintf("-J%s=%s", value.Name, value.Value))
	}

	runPath := basePath
	if workingDir != "" {
		runPath = filepath.Join(basePath, "repo", workingDir)
	}

	outputDir := filepath.Join(runPath, "output")
	// clean output directory it already exists, only useful for local development
	_, err = os.Stat(outputDir)
	if err == nil {
		if err := os.RemoveAll(outputDir); err != nil {
			output.PrintLog(fmt.Sprintf("%s Failed to clean output directory %s", ui.IconWarning, outputDir))
		}
	}
	// recreate output directory with wide permissions so JMeter can create report files
	if err := os.Mkdir(outputDir, 0777); err != nil {
		return *result.Err(fmt.Errorf("could not create directory %s: %w", runPath, err)), nil
	}

	jtlPath := filepath.Join(outputDir, "report.jtl")
	reportPath := filepath.Join(outputDir, "report")
	args := []string{"-n", "-t", path, "-l", jtlPath, "-e", "-o", reportPath}
	args = append(args, params...)

	// append args from execution
	args = append(args, execution.Args...)
	output.PrintLog(fmt.Sprintf("%s Using arguments: %v", ui.IconWorld, args))

	// run JMeter inside repo directory ignore execution error in case of failed test
	out, err := executor.Run(runPath, "jmeter", envManager, args...)
	if err != nil {
		return *result.WithErrors(fmt.Errorf("jmeter run error: %w", err)), nil
	}
	out = envManager.ObfuscateSecrets(out)

	output.PrintLog(fmt.Sprintf("%s Getting report %s", ui.IconFile, jtlPath))
	f, err := os.Open(jtlPath)
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
			outputDir,
		}

		output.PrintLog(fmt.Sprintf("Scraping directories: %v", directories))

		if err := r.scrape(context.Background(), directories, execution); err != nil {
			return *executionResult.WithErrors(fmt.Errorf("scrape artifacts error: %w", err)), nil
		}
	}

	return executionResult, nil
}

func (r *JMeterRunner) scrape(ctx context.Context, dirs []string, execution testkube.Execution) (err error) {
	if !r.Params.ScrapperEnabled {
		return nil
	}

	output.PrintLog(fmt.Sprintf("%s Extracting artifacts from %s using filesystem extractor", ui.IconCheckMark, dirs))
	extractor := scraper.NewFilesystemExtractor(dirs, filesystem.NewOSFileSystem())
	var loader scraper.Uploader
	var meta map[string]any
	if r.Params.CloudMode {
		output.PrintLog(fmt.Sprintf("%s Uploading artifacts using Cloud uploader", ui.IconCheckMark))
		meta = cloudscraper.ExtractCloudLoaderMeta(execution)
		cloudExecutor := cloudexecutor.NewCloudGRPCExecutor(r.CloudClient, r.Params.CloudAPIKey)
		loader = cloudscraper.NewCloudUploader(cloudExecutor)
	} else {
		output.PrintLog(fmt.Sprintf("%s Uploading artifacts using MinIO uploader", ui.IconCheckMark))
		meta = scraper.ExtractMinIOLoaderMeta(execution)
		loader, err = scraper.NewMinIOLoader(
			r.Params.Endpoint,
			r.Params.AccessKeyID,
			r.Params.SecretAccessKey,
			r.Params.Location,
			r.Params.Token,
			r.Params.Bucket,
			r.Params.Ssl,
		)
		if err != nil {
			return err
		}
	}
	scraperV2 := scraper.NewScraperV2(extractor, loader)
	if err = scraperV2.Scrape(ctx, meta); err != nil {
		output.PrintLog(fmt.Sprintf("%s Error encountered while scraping artifacts", ui.IconCross))
		return errors.Errorf("scrape artifacts error: %v", err)
	}

	return nil
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
