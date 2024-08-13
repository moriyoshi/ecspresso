package ecspresso

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/go-jsonnet"
	goVersion "github.com/hashicorp/go-version"
	"github.com/kayac/ecspresso/v2/appspec"
	goConfig "github.com/kayac/go-config"
	"github.com/samber/lo"
)

const (
	DefaultClusterName = "default"
	DefaultTimeout     = 10 * time.Minute
)

var awsv2ConfigLoadOptionsFunc []func(*awsConfig.LoadOptions) error

type configLoader struct {
	*goConfig.Loader
	VM *jsonnet.VM
}

func newConfigLoader(extStr, extCode map[string]string) *configLoader {
	vm := jsonnet.MakeVM()
	for k, v := range extStr {
		vm.ExtVar(k, v)
	}
	for k, v := range extCode {
		vm.ExtCode(k, v)
	}
	for _, f := range DefaultJsonnetNativeFuncs() {
		vm.NativeFunction(f)
	}
	return &configLoader{
		Loader: goConfig.New(),
		VM:     vm,
	}
}

func (c *configLoader) withExtraEnvSet(
	extraEnv map[string]string,
	f func() ([]byte, error),
) ([]byte, error) {
	if extraEnv != nil {
		oldValues := make(map[string]interface{})
		for k, v := range extraEnv {
			if ov, ok := os.LookupEnv(k); ok {
				oldValues[k] = ov
			} else {
				oldValues[k] = nil
			}
			os.Setenv(k, v)
		}
		defer func() {
			for k, v := range oldValues {
				if v == nil {
					os.Unsetenv(k)
				} else {
					os.Setenv(k, v.(string))
				}
			}
		}()
	}
	return f()
}

func (c *configLoader) ReadWithEnv(configPath string, extraEnv map[string]string) ([]byte, error) {
	return c.withExtraEnvSet(extraEnv, func() ([]byte, error) {
		return c.Loader.ReadWithEnv(configPath)
	})
}

func (c *configLoader) ReadWithEnvBytes(b []byte, extraEnv map[string]string) ([]byte, error) {
	return c.withExtraEnvSet(extraEnv, func() ([]byte, error) {
		return c.Loader.ReadWithEnvBytes(b)
	})
}

// Config represents a configuration.
type Config struct {
	RequiredVersion       string            `yaml:"required_version,omitempty" json:"required_version,omitempty"`
	Region                string            `yaml:"region" json:"region"`
	Cluster               string            `yaml:"cluster" json:"cluster"`
	Service               string            `yaml:"service" json:"service"`
	ServiceDefinitionPath string            `yaml:"service_definition" json:"service_definition"`
	TaskDefinitionPath    string            `yaml:"task_definition" json:"task_definition"`
	Plugins               []ConfigPlugin    `yaml:"plugins,omitempty" json:"plugins,omitempty"`
	AppSpec               *appspec.AppSpec  `yaml:"appspec,omitempty" json:"appspec,omitempty"`
	FilterCommand         string            `yaml:"filter_command,omitempty" json:"filter_command,omitempty"`
	Timeout               *Duration         `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	CodeDeploy            *ConfigCodeDeploy `yaml:"codedeploy,omitempty" json:"codedeploy,omitempty"`
	Ignore                *ConfigIgnore     `yaml:"ignore,omitempty" json:"ignore,omitempty"`
	Env                   map[string]string `yaml:"env,omitempty" json:"env,ignore,omitempty"`

	path               string
	templateFuncs      []template.FuncMap
	jsonnetNativeFuncs []*jsonnet.NativeFunction
	dir                string
	versionConstraints goVersion.Constraints
	awsv2Config        aws.Config
}

type ConfigCodeDeploy struct {
	ApplicationName     string `yaml:"application_name,omitempty" json:"application_name,omitempty"`
	DeploymentGroupName string `yaml:"deployment_group_name,omitempty" json:"deployment_group_name,omitempty"`
}

// Load loads configuration file from file path.
func (l *configLoader) Load(ctx context.Context, path string, version string) (*Config, error) {
	conf := &Config{path: path}
	ext := filepath.Ext(path)
	switch ext {
	case ymlExt, yamlExt:
		b, err := l.ReadWithEnv(path, nil)
		if err != nil {
			return nil, err
		}
		if err := unmarshalYAML(b, conf, path); err != nil {
			return nil, fmt.Errorf("failed to parse yaml: %w", err)
		}
	case jsonExt, jsonnetExt:
		jsonStr, err := l.VM.EvaluateFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate jsonnet file: %w", err)
		}
		b, err := l.ReadWithEnvBytes([]byte(jsonStr), nil)
		if err != nil {
			return nil, fmt.Errorf("failed to read template file: %w", err)
		}
		if err := unmarshalJSON(b, conf, path); err != nil {
			return nil, fmt.Errorf("failed to unmarshal json: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported config file extension: %s", ext)
	}

	conf.dir = filepath.Dir(path)
	if err := conf.Restrict(ctx); err != nil {
		return nil, err
	}
	if err := conf.ValidateVersion(version); err != nil {
		return nil, err
	}
	for _, f := range conf.templateFuncs {
		l.Funcs(f)
	}
	for _, f := range conf.jsonnetNativeFuncs {
		l.VM.NativeFunction(f)
	}
	return conf, nil
}

func (c *Config) OverrideByCLIOptions(opt *CLIOptions) {
	if opt.Timeout != nil {
		c.Timeout = &Duration{*opt.Timeout}
	}
	if opt.FilterCommand != "" {
		c.FilterCommand = opt.FilterCommand
	}
}

// Restrict restricts a configuration.
func (c *Config) Restrict(ctx context.Context) error {
	if c.Cluster == "" {
		c.Cluster = DefaultClusterName
	}
	if c.dir == "" {
		c.dir = "."
	}
	if c.ServiceDefinitionPath != "" && !filepath.IsAbs(c.ServiceDefinitionPath) {
		c.ServiceDefinitionPath = filepath.Join(c.dir, c.ServiceDefinitionPath)
	}
	if c.TaskDefinitionPath != "" && !filepath.IsAbs(c.TaskDefinitionPath) {
		c.TaskDefinitionPath = filepath.Join(c.dir, c.TaskDefinitionPath)
	}
	if c.RequiredVersion != "" {
		constraints, err := goVersion.NewConstraint(c.RequiredVersion)
		if err != nil {
			return fmt.Errorf("required_version has invalid format: %w", err)
		}
		c.versionConstraints = constraints
	}
	if c.Timeout == nil {
		c.Timeout = &Duration{Duration: DefaultTimeout}
	}
	if c.Region == "" {
		c.Region = os.Getenv("AWS_REGION")
	}
	var err error
	var optsFunc []func(*awsConfig.LoadOptions) error
	if len(awsv2ConfigLoadOptionsFunc) == 0 {
		// default
		// Log("[INFO] use default aws config load options")
		optsFunc = []func(*awsConfig.LoadOptions) error{
			awsConfig.WithRegion(c.Region),
		}
	} else {
		// Log("[INFO] override aws config load options")
		optsFunc = awsv2ConfigLoadOptionsFunc
	}
	c.awsv2Config, err = awsConfig.LoadDefaultConfig(ctx, optsFunc...)
	if err != nil {
		return fmt.Errorf("failed to load aws config: %w", err)
	}
	if err := c.setupPlugins(ctx); err != nil {
		return fmt.Errorf("failed to setup plugins: %w", err)
	}
	if c.FilterCommand != "" {
		Log("[WARNING] filter_command is deprecated. Use environment variable or CLI flag instead.")
	}
	return nil
}

func (c *Config) AssumeRole(assumeRoleARN string) {
	if assumeRoleARN == "" {
		return
	}
	Log("[INFO] assume role: %s", assumeRoleARN)
	stsClient := sts.NewFromConfig(c.awsv2Config)
	assumeRoleProvider := stscreds.NewAssumeRoleProvider(stsClient, assumeRoleARN)
	c.awsv2Config.Credentials = aws.NewCredentialsCache(assumeRoleProvider)
}

func (c *Config) setupPlugins(ctx context.Context) error {
	plugins := []ConfigPlugin{}
	for _, name := range defaultPluginNames {
		plugins = append(plugins, ConfigPlugin{Name: name})
	}
	plugins = append(plugins, c.Plugins...)
	for _, p := range plugins {
		if err := p.Setup(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// ValidateVersion validates a version satisfies required_version.
func (c *Config) ValidateVersion(version string) error {
	if c.versionConstraints == nil {
		return nil
	}
	v, err := goVersion.NewVersion(version)
	if err != nil {
		Log("[WARNING] Invalid version format \"%s\". Skip checking required_version.", version)
		// invalid version string (e.g. "current") always allowed
		return nil
	}
	if !c.versionConstraints.Check(v) {
		return fmt.Errorf("version %s does not satisfy constraints required_version: %s", version, c.versionConstraints)
	}

	return nil
}

// NewDefaultConfig creates a default configuration.
func NewDefaultConfig() *Config {
	return &Config{
		Region:  os.Getenv("AWS_REGION"),
		Timeout: &Duration{DefaultTimeout},
	}
}

type ConfigIgnore struct {
	Tags []string `yaml:"tags,omitempty" json:"tags,omitempty"`
}

type hasTags interface {
	GetTags() []types.Tag
	SetTags([]types.Tag)
}

func (i *ConfigIgnore) filterTags(tags []types.Tag) []types.Tag {
	if i == nil || len(i.Tags) == 0 {
		return tags
	}
	return lo.Filter(tags, func(tag types.Tag, _ int) bool {
		return !lo.Contains(i.Tags, aws.ToString(tag.Key))
	})
}

func (i *ConfigIgnore) Apply(v hasTags) error {
	v.SetTags(i.filterTags(v.GetTags()))
	return nil
}
