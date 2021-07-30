package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"github.com/hashicorp/go-multierror"
	"github.com/honeycombio/libhoney-go"
	"github.com/inconshreveable/log15"

	"github.com/sourcegraph/sourcegraph/enterprise/cmd/executor/internal/command"
	"github.com/sourcegraph/sourcegraph/enterprise/cmd/executor/internal/ignite"
	"github.com/sourcegraph/sourcegraph/enterprise/cmd/executor/internal/janitor"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/executor"
	"github.com/sourcegraph/sourcegraph/internal/honey"
	"github.com/sourcegraph/sourcegraph/internal/workerutil"
)

type handler struct {
	nameSet       *janitor.NameSet
	store         workerutil.Store
	options       Options
	operations    *command.Operations
	runnerFactory func(dir string, logger *command.Logger, options command.Options, operations *command.Operations) command.Runner
}

var _ workerutil.Handler = &handler{}
var _ workerutil.WithPreDequeue = &handler{}

// PreDequeue determines if the number of VMs with the current instance's VM Prefix is less than
// the maximum number of concurrent handlers. If so, then a new job can be dequeued. Otherwise,
// we have an orphaned VM somewhere on the host that will be cleaned up by the background janitor
// process - refuse to dequeue a job for now so that we do not over-commit on VMs and cause issues
// with keeping our heartbeats due to machine load. We'll continue to check this condition on the
// polling interval
func (h *handler) PreDequeue(ctx context.Context) (dequeueable bool, extraDequeueArguments interface{}, err error) {
	if !h.options.FirecrackerOptions.Enabled {
		return true, nil, nil
	}

	currentVMs, err := ignite.CurrentlyRunningVMs(context.Background(), h.options.VMPrefix)
	if err != nil {
		return false, nil, err
	}

	if len(currentVMs) < h.options.WorkerOptions.NumHandlers {
		return true, nil, nil
	}

	log15.Warn("Orphaned VMs detected - refusing to dequeue a new job until it's cleaned up", "numCurrentVMs", len(currentVMs), "numHandlers", h.options.WorkerOptions.NumHandlers)
	return false, nil, nil
}

// Handle clones the target code into a temporary directory, invokes the target indexer in a
// fresh docker container, and uploads the results to the external frontend API.
func (h *handler) Handle(ctx context.Context, record workerutil.Record) (err error) {
	job := record.(executor.Job)
	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(h.options.MaximumRuntimePerJob))
	defer cancel()

	wrapError := func(err error, message string) error {
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.Errorf("job exceeded maximum execution time of %s", h.options.MaximumRuntimePerJob)
		}

		return errors.Wrap(err, message)
	}

	start := time.Now()
	defer func() {
		if honey.Enabled() {
			_ = createHoneyEvent(ctx, job, err, time.Since(start)).Send()
		}
	}()

	// 🚨 SECURITY: The job logger must be supplied with all sensitive values that may appear
	// in a command constructed and run in the following function. Note that the command and
	// its output may both contain sensitive values, but only values which we directly
	// interpolate into the command. No command that we run on the host leaks environment
	// variables, and the user-specified commands (which could leak their environment) are
	// run in a clean VM.
	logger := command.NewLogger(h.store, job, record.RecordID(), union(h.options.RedactedValues, job.RedactedValues))
	defer logger.Flush()

	// Create a working directory for this job which will be removed once the job completes.
	// If a repository is supplied as part of the job configuration, it will be cloned into
	// the working directory.

	log15.Info("Creating workspace", "jobID", job.ID, "repositoryName", job.RepositoryName, "commit", job.Commit)

	hostRunner := h.runnerFactory("", logger, command.Options{}, h.operations)
	workingDirectory, err := h.prepareWorkspace(ctx, hostRunner, job.RepositoryName, job.Commit)
	if err != nil {
		return wrapError(err, "failed to prepare workspace")
	}
	defer func() {
		_ = os.RemoveAll(workingDirectory)
	}()

	// Copy the file contents from the job record into the working directory
	for relativePath, content := range job.VirtualMachineFiles {
		path, err := filepath.Abs(filepath.Join(workingDirectory, relativePath))
		if err != nil {
			return err
		}

		if !strings.HasPrefix(path, workingDirectory) {
			return errors.Errorf("refusing to write outside of working directory")
		}

		if err := os.WriteFile(path, []byte(content), os.ModePerm); err != nil {
			return err
		}
	}

	vmNameSuffix, err := uuid.NewRandom()
	if err != nil {
		return err
	}

	// Construct a unique name for the VM prefixed by something that differentiates
	// VMs created by this executor instance and another one that happens to run on
	// the same host (as is the case in dev). This prefix is expected to match the
	// prefix given to ignite.CurrentlyRunningVMs in other parts of this service.
	name := fmt.Sprintf("%s-%s", h.options.VMPrefix, vmNameSuffix.String())

	// Before we setup a VM (and after we teardown), mark the name as in-use so that
	// the janitor process cleaning up orphaned VMs doesn't try to stop/remove the one
	// we're using for the current job.
	h.nameSet.Add(name)
	defer h.nameSet.Remove(name)

	options := command.Options{
		ExecutorName:       name,
		FirecrackerOptions: h.options.FirecrackerOptions,
		ResourceOptions:    h.options.ResourceOptions,
	}
	runner := h.runnerFactory(workingDirectory, logger, options, h.operations)

	// Deduplicate and sort (for testing)
	imageMap := map[string]struct{}{}
	for _, dockerStep := range job.DockerSteps {
		imageMap[dockerStep.Image] = struct{}{}
	}

	imageNames := make([]string, 0, len(imageMap))
	for image := range imageMap {
		imageNames = append(imageNames, image)
	}
	sort.Strings(imageNames)

	scriptNames := make([]string, 0, len(job.DockerSteps))
	for i, dockerStep := range job.DockerSteps {
		scriptName := scriptNameFromJobStep(job, i)
		scriptPath := filepath.Join(workingDirectory, command.ScriptsPath, scriptName)

		if err := os.WriteFile(scriptPath, buildScript(dockerStep), os.ModePerm); err != nil {
			return err
		}

		scriptNames = append(scriptNames, scriptName)
	}

	log15.Info("Setting up VM", "jobID", job.ID, "repositoryName", job.RepositoryName, "commit", job.Commit)

	// Setup Firecracker VM (if enabled)
	if err := runner.Setup(ctx, imageNames, nil); err != nil {
		return wrapError(err, "failed to setup virtual machine")
	}
	defer func() {
		// Perform this outside of the task execution context. If there is a timeout or
		// cancellation error we don't want to skip cleaning up the resources that we've
		// allocated for the current task.
		if teardownErr := runner.Teardown(context.Background()); teardownErr != nil {
			err = multierror.Append(err, teardownErr)
		}
	}()

	// Invoke each docker step sequentially
	for i, dockerStep := range job.DockerSteps {
		dockerStepCommand := command.CommandSpec{
			Key:        fmt.Sprintf("step.docker.%d", i),
			Image:      dockerStep.Image,
			ScriptPath: scriptNames[i],
			Dir:        dockerStep.Dir,
			Env:        dockerStep.Env,
			Operation:  h.operations.Exec,
		}

		log15.Info(fmt.Sprintf("Running docker step #%d", i), "jobID", job.ID, "repositoryName", job.RepositoryName, "commit", job.Commit)

		if err := runner.Run(ctx, dockerStepCommand); err != nil {
			return wrapError(err, "failed to perform docker step")
		}
	}

	// Invoke each src-cli step sequentially
	for i, cliStep := range job.CliSteps {
		log15.Info(fmt.Sprintf("Running src-cli step #%d", i), "jobID", job.ID, "repositoryName", job.RepositoryName, "commit", job.Commit)

		cliStepCommand := command.CommandSpec{
			Key:       fmt.Sprintf("step.src.%d", i),
			Command:   append([]string{"src"}, cliStep.Commands...),
			Dir:       cliStep.Dir,
			Env:       cliStep.Env,
			Operation: h.operations.Exec,
		}

		if err := runner.Run(ctx, cliStepCommand); err != nil {
			return wrapError(err, "failed to perform src-cli step")
		}
	}

	return nil
}

var scriptPreamble = `
set -x
`

func buildScript(dockerStep executor.DockerStep) []byte {
	return []byte(strings.Join(append([]string{scriptPreamble, ""}, dockerStep.Commands...), "\n") + "\n")
}

func union(a, b map[string]string) map[string]string {
	c := make(map[string]string, len(a)+len(b))

	for k, v := range a {
		c[k] = v
	}
	for k, v := range b {
		c[k] = v
	}

	return c
}

func scriptNameFromJobStep(job executor.Job, i int) string {
	return fmt.Sprintf("%d.%d_%s@%s.sh", job.ID, i, strings.ReplaceAll(job.RepositoryName, "/", "_"), job.Commit)
}

func createHoneyEvent(ctx context.Context, job executor.Job, err error, duration time.Duration) *libhoney.Event {
	fields := map[string]interface{}{
		"duration_ms":    duration.Milliseconds(),
		"recordID":       job.RecordID(),
		"repositoryName": job.RepositoryName,
		"commit":         job.Commit,
		"numDockerSteps": len(job.DockerSteps),
		"numCliSteps":    len(job.CliSteps),
	}

	if err != nil {
		fields["error"] = err.Error()
	}
	// Currently disabled as the import pulls in conf packages
	// if spanURL := trace.SpanURLFromContext(ctx); spanURL != "" {
	// 	fields["trace"] = spanURL
	// }

	return honey.EventWithFields("executor", fields)
}
