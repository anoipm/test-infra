package main

import (
	"bufio"
	"flag"
	"fmt"
	"github.com/kyma-project/test-infra/development/pkg/sets"
	"github.com/kyma-project/test-infra/development/pkg/tags"
	"io"
	"io/fs"
	errutil "k8s.io/apimachinery/pkg/util/errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type options struct {
	Config
	configPath string
	context    string
	dockerfile string
	envFile    string
	name       string
	variant    string
	logDir     string
	silent     bool
	isCI       bool
	tags       sets.Strings
	platforms  sets.Strings
}

const (
	PlatformLinuxAmd64 = "linux/amd64"
	PlatformLinuxArm64 = "linux/arm64"
)

// parseVariable returns a build-arg.
// Keys are set to upper-case.
func parseVariable(key, value string) string {
	k := strings.TrimSpace(key)
	return k + "=" + strings.TrimSpace(value)
}

// runInBuildKit prepares command execution and handles gathering logs from BuildKit-enabled run
// This function is used only in customized environment
func runInBuildKit(o options, name string, destinations, platforms []string, buildArgs map[string]string) error {
	dockerfile := filepath.Base(o.dockerfile)
	dockerfileDir := filepath.Dir(o.dockerfile)
	args := []string{
		"build", "--frontend=dockerfile.v0",
		"--local", "context=" + o.context,
		"--local", "dockerfile=" + filepath.Join(o.context, dockerfileDir),
		"--opt", "filename=" + dockerfile,
	}

	// output definition, multiple images support
	args = append(args, "--output", "type=image,\"name="+strings.Join(destinations, ",")+"\",push=true")

	// build-args
	for k, v := range buildArgs {
		args = append(args, "--opt", "build-arg:"+parseVariable(k, v))
	}

	if len(platforms) > 0 {
		args = append(args, "--opt", "platform="+strings.Join(platforms, ","))
	}

	if o.Cache.Enabled {
		// TODO (@Ressetkk): Implement multiple caches, see https://github.com/moby/buildkit#export-cache
		args = append(args,
			"--export-cache", "type=registry,ref="+o.Cache.CacheRepo,
			"--import-cache", "type=registry,"+o.Cache.CacheRepo)
	}

	cmd := exec.Command("buildctl-daemonless.sh", args...)

	var outw []io.Writer
	var errw []io.Writer

	if !o.silent {
		outw = append(outw, os.Stdout)
		errw = append(errw, os.Stderr)
	}

	f, err := os.Create(filepath.Join(o.logDir, strings.TrimSpace("build_"+strings.TrimSpace(name)+".log")))
	if err != nil {
		return fmt.Errorf("could not create log file: %w", err)
	}

	outw = append(outw, f)
	errw = append(errw, f)

	cmd.Stdout = io.MultiWriter(outw...)
	cmd.Stderr = io.MultiWriter(errw...)

	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

// runInKaniko prepares command execution and handles gathering logs to file
func runInKaniko(o options, name string, destinations, platforms []string, buildArgs map[string]string) error {
	args := []string{
		"--context=" + o.context,
		"--dockerfile=" + o.dockerfile,
	}
	for _, dst := range destinations {
		args = append(args, "--destination="+dst)
	}

	for k, v := range buildArgs {
		args = append(args, "--build-arg="+parseVariable(k, v))
	}

	if len(platforms) > 0 {
		fmt.Println("'--platform' parameter not supported in kaniko-mode. Use buildkit-enabled image")
	}

	if o.Config.Cache.Enabled {
		args = append(args, "--cache="+strconv.FormatBool(o.Cache.Enabled),
			"--cache-copy-layers="+strconv.FormatBool(o.Cache.CacheCopyLayers),
			"--cache-run-layers="+strconv.FormatBool(o.Cache.CacheRunLayers),
			"--cache-repo="+o.Cache.CacheRepo)
	}

	if o.Config.LogFormat != "" {
		args = append(args, "--log-format="+o.Config.LogFormat)
	}

	if o.Config.Reproducible {
		args = append(args, "--reproducible=true")
	}

	cmd := exec.Command("/kaniko/executor", args...)

	var outw []io.Writer
	var errw []io.Writer

	if !o.silent {
		outw = append(outw, os.Stdout)
		errw = append(errw, os.Stderr)
	}

	f, err := os.Create(filepath.Join(o.logDir, strings.TrimSpace("build_"+strings.TrimSpace(name)+".log")))
	if err != nil {
		return fmt.Errorf("could not create log file: %w", err)
	}

	outw = append(outw, f)
	errw = append(errw, f)

	cmd.Stdout = io.MultiWriter(outw...)
	cmd.Stderr = io.MultiWriter(errw...)

	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func runBuildJob(o options, vs Variants) error {
	runFunc := runInKaniko
	if os.Getenv("USE_BUILDKIT") == "true" {
		runFunc = runInBuildKit
	}
	var sha, pr string
	var err error
	repo := o.Config.Registry
	if o.isCI {
		presubmit := os.Getenv("JOB_TYPE") == "presubmit"
		if presubmit {
			if len(o.DevRegistry) > 0 {
				repo = o.DevRegistry
			}
			if n := os.Getenv("PULL_NUMBER"); n != "" {
				pr = n
			}
		}

		if c := os.Getenv("PULL_BASE_SHA"); c != "" {
			sha = c
		}
	}

	// if sha is still not set, fail the pipeline
	if sha == "" {
		return fmt.Errorf("'sha' could not be determined")
	}

	parsedTags, err := getTags(pr, sha, append(o.tags, o.TagTemplate))
	if err != nil {
		return err
	}
	envMap, err := loadEnv(os.DirFS("/"), o.envFile)
	if err != nil {
		return fmt.Errorf("load env: %w", err)
	}
	if len(vs) == 0 {
		// variants.yaml file not present or either empty. Run single build.
		destinations := gatherDestinations(repo, o.name, parsedTags)
		fmt.Println("Starting build for image: ", strings.Join(destinations, ", "))
		err = runFunc(o, "build", destinations, o.platforms, envMap)
		if err != nil {
			return fmt.Errorf("build encountered error: %w", err)
		}
		fmt.Println("Successfully built image:", strings.Join(destinations, ", "))
	}

	var errs []error
	for variant, env := range vs {
		var variantTags []string
		for _, tag := range parsedTags {
			variantTags = append(variantTags, tag+"-"+variant)
		}
		destinations := gatherDestinations(repo, o.name, variantTags)
		fmt.Println("Starting build for image: ", strings.Join(destinations, ", "))
		// (@Ressetkk): When variants provided, build doesn't use env files.
		// Similar logic should be provided once variants are fixed.
		if err := runFunc(o, variant, destinations, o.platforms, env); err != nil {
			errs = append(errs, fmt.Errorf("job %s ended with error: %w", variant, err))
			fmt.Printf("Job '%s' ended with error: %s.\n", variant, err)
		} else {
			fmt.Println("Successfully built image:", strings.Join(destinations, ", "))
			fmt.Printf("Job '%s' finished successfully.\n", variant)
		}
	}
	return errutil.NewAggregate(errs)
}

func getTags(pr, sha string, templates []string) ([]string, error) {
	// (Ressetkk): PR tag should not be hardcoded, in the future we have to find a way to parametrize it
	if pr != "" {
		// assume we are using PR number, build tag as 'PR-XXXX'
		return []string{"PR-" + pr}, nil
	}
	// build a tag from commit SHA
	tagger, err := tags.NewTagger(templates, tags.CommitSHA(sha))
	if err != nil {
		return nil, fmt.Errorf("get tagger: %w", err)
	}
	p, err := tagger.ParseTags()
	if err != nil {
		return nil, fmt.Errorf("build tag: %w", err)
	}
	return p, nil
}

func gatherDestinations(repo []string, name string, tags []string) []string {
	var dst []string
	for _, t := range tags {
		for _, r := range repo {
			image := path.Join(r, name)
			dst = append(dst, image+":"+strings.ReplaceAll(t, " ", "-"))
		}
	}
	return dst
}

// validateOptions handles options validation. All checks should be provided here
func validateOptions(o options) error {
	var errs []error
	if o.context == "" {
		errs = append(errs, fmt.Errorf("flag '--context' is missing"))
	}
	if o.name == "" {
		errs = append(errs, fmt.Errorf("flag '--name' is missing"))
	}
	if o.dockerfile == "" {
		errs = append(errs, fmt.Errorf("flag '--dockerfile' is missing"))
	}
	return errutil.NewAggregate(errs)
}

// loadEnv loads environment variables into application runtime from key=value list
func loadEnv(vfs fs.FS, envFile string) (map[string]string, error) {
	f, err := vfs.Open(envFile)
	if err != nil {
		return nil, fmt.Errorf("open env file: %w", err)
	}
	s := bufio.NewScanner(f)
	vars := make(map[string]string)
	for s.Scan() {
		kv := s.Text()
		sp := strings.SplitN(kv, "=", 2)
		key, val := sp[0], sp[1]
		if len(sp) > 2 {
			return nil, fmt.Errorf("env var split incorrectly: 2 != %v", len(sp))
		}
		if _, ok := os.LookupEnv(key); ok {
			// do not override env variable if it's already present in the runtime
			// do not include in vars map since dev should not have access to it anyway
			continue
		}
		err := os.Setenv(key, val)
		if err != nil {
			return nil, fmt.Errorf("setenv: %w", err)
		}
		// add value to the vars that will be injected as build args
		vars[key] = val
	}
	return vars, nil
}

func (o *options) gatherOptions(fs *flag.FlagSet) *flag.FlagSet {
	fs.BoolVar(&o.silent, "silent", false, "Do not push build logs to stdout")
	fs.StringVar(&o.configPath, "config", "/config/image-builder-config.yaml", "Path to application config file")
	fs.StringVar(&o.context, "context", ".", "Path to build directory context")
	fs.StringVar(&o.envFile, "env-file", "", "Path to file with environment variables to be loaded in build")
	fs.StringVar(&o.name, "name", "", "Name of the image to be built")
	fs.StringVar(&o.dockerfile, "dockerfile", "Dockerfile", "Path to Dockerfile file relative to context")
	fs.StringVar(&o.variant, "variant", "", "If variants.yaml file is present, define which variant should be built. If variants.yaml is not present, this flag will be ignored")
	fs.StringVar(&o.logDir, "log-dir", "/logs/artifacts", "Path to logs directory where GCB logs will be stored")
	fs.Var(&o.tags, "tag", "Additional tag that the image will be tagged")
	fs.Var(&o.platforms, "platform", "Only supported with BuildKit. Platform of the image that is built")
	return fs
}

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	o := options{isCI: os.Getenv("CI") == "true"}
	o.gatherOptions(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if o.configPath == "" {
		fmt.Println("'--config' flag is missing or has empty value, please provide the path to valid 'config.yaml' file")
		os.Exit(1)
	}
	c, err := os.ReadFile(o.configPath)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := o.ParseConfig(c); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// validate if options provided by flags and config file are fine
	if err := validateOptions(o); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	context, err := filepath.Abs(o.context)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	variantsFile := filepath.Join(context, filepath.Dir(o.dockerfile), "variants.yaml")
	variant, err := GetVariants(o.variant, variantsFile, os.ReadFile)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Println(err)
			os.Exit(1)
		}
	}
	err = runBuildJob(o, variant)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("Job's done.")
}
