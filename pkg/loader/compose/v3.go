/*
Copyright 2017 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package compose

import (
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cast"

	compose "github.com/compose-spec/compose-go/types"
	api "k8s.io/api/core/v1"

	"github.com/google/shlex"
	"github.com/kubernetes/kompose/pkg/kobject"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

func loadV3Placement(placement compose.Placement) kobject.Placement {
	komposePlacement := kobject.Placement{
		PositiveConstraints: make(map[string]string),
		NegativeConstraints: make(map[string]string),
	}
	equal, notEqual := " == ", " != "
	errMsg := " constraints in placement is not supported, only 'node.hostname', 'engine.labels.operatingsystem' and 'node.labels.xxx' (ex: node.labels.something == anything) is supported as a constraint "
	for _, j := range placement.Constraints {
		operator := equal
		if strings.Contains(j, notEqual) {
			operator = notEqual
		}
		p := strings.Split(j, operator)
		if len(p) < 2 {
			log.Warn(p[0], errMsg)
			continue
		}

		var key string
		if p[0] == "node.hostname" {
			key = "kubernetes.io/hostname"
		} else if p[0] == "engine.labels.operatingsystem" {
			key = "beta.kubernetes.io/os"
		} else if strings.HasPrefix(p[0], "node.labels.") {
			key = strings.TrimPrefix(p[0], "node.labels.")
		} else {
			log.Warn(p[0], errMsg)
			continue
		}

		if operator == equal {
			komposePlacement.PositiveConstraints[key] = p[1]
		} else if operator == notEqual {
			komposePlacement.NegativeConstraints[key] = p[1]
		}
	}
	return komposePlacement
}

// Convert the Docker Compose v3 volumes to []string (the old way)
// TODO: Check to see if it's a "bind" or "volume". Ignore for now.
// TODO: Refactor it similar to loadV3Ports
// See: https://docs.docker.com/compose/compose-file/#long-syntax-3
func loadV3Volumes(volumes []compose.ServiceVolumeConfig) []string {
	var volArray []string
	for _, vol := range volumes {
		// There will *always* be Source when parsing
		v := vol.Source

		if vol.Target != "" {
			v = v + ":" + vol.Target
		}

		if vol.ReadOnly {
			v = v + ":ro"
		}

		volArray = append(volArray, v)
	}
	return volArray
}

// Convert Docker Compose v3 ports to kobject.Ports
// expose ports will be treated as TCP ports
func loadV3Ports(ports []compose.ServicePortConfig, expose []string) []kobject.Ports {
	komposePorts := []kobject.Ports{}

	exist := map[string]bool{}

	for _, port := range ports {
		// Convert to a kobject struct with ports
		// NOTE: V3 doesn't use IP (they utilize Swarm instead for host-networking).
		// Thus, IP is blank.
		komposePorts = append(komposePorts, kobject.Ports{
			HostPort:      int32(port.Published),
			ContainerPort: int32(port.Target),
			HostIP:        "",
			Protocol:      strings.ToUpper(port.Protocol),
		})

		exist[cast.ToString(port.Target)+port.Protocol] = true
	}

	if expose != nil {
		for _, port := range expose {
			portValue := port
			protocol := string(api.ProtocolTCP)
			if strings.Contains(portValue, "/") {
				splits := strings.Split(port, "/")
				portValue = splits[0]
				protocol = splits[1]
			}

			if exist[portValue+protocol] {
				continue
			}
			komposePorts = append(komposePorts, kobject.Ports{
				HostPort:      cast.ToInt32(portValue),
				ContainerPort: cast.ToInt32(portValue),
				HostIP:        "",
				Protocol:      strings.ToUpper(protocol),
			})
		}
	}

	return komposePorts
}

/* Convert the HealthCheckConfig as designed by Docker to
a Kubernetes-compatible format.
*/
func parseHealthCheckReadiness(labels compose.Labels) (kobject.HealthCheck, error) {
	// initialize with CMD as default to not break at return (will be ignored if no test is informed)
	test := []string{"CMD"}
	var timeout, interval, retries, startPeriod int32
	var disable bool

	for key, value := range labels {
		switch key {
		case HealthCheckReadinessDisable:
			disable = cast.ToBool(value)
		case HealthCheckReadinessTest:
			if len(value) > 0 {
				test, _ = shlex.Split(value)
			}
		case HealthCheckReadinessInterval:
			parse, err := time.ParseDuration(value)
			if err != nil {
				return kobject.HealthCheck{}, errors.Wrap(err, "unable to parse health check interval variable")
			}
			interval = int32(parse.Seconds())
		case HealthCheckReadinessTimeout:
			parse, err := time.ParseDuration(value)
			if err != nil {
				return kobject.HealthCheck{}, errors.Wrap(err, "unable to parse health check timeout variable")
			}
			timeout = int32(parse.Seconds())
		case HealthCheckReadinessRetries:
			retries = cast.ToInt32(value)
		case HealthCheckReadinessStartPeriod:
			parse, err := time.ParseDuration(value)
			if err != nil {
				return kobject.HealthCheck{}, errors.Wrap(err, "unable to parse health check startPeriod variable")
			}
			startPeriod = int32(parse.Seconds())
		}
	}

	if test[0] == "NONE" {
		disable = true
		test = test[1:]
	}
	if test[0] == "CMD" || test[0] == "CMD-SHELL" {
		test = test[1:]
	}

	// Due to docker/cli adding "CMD-SHELL" to the struct, we remove the first element of composeHealthCheck.Test
	return kobject.HealthCheck{
		Test:        test,
		Timeout:     timeout,
		Interval:    interval,
		Retries:     retries,
		StartPeriod: startPeriod,
		Disable:     disable,
	}, nil
}

/* Convert the HealthCheckConfig as designed by Docker to
a Kubernetes-compatible format.
*/
func parseHealthCheck(composeHealthCheck compose.HealthCheckConfig, labels compose.Labels) (kobject.HealthCheck, error) {
	var timeout, interval, retries, startPeriod int32
	var test []string
	var httpPort int32
	var httpPath string

	// Here we convert the timeout from 1h30s (example) to 36030 seconds.
	if composeHealthCheck.Timeout != nil {
		parse, err := time.ParseDuration(composeHealthCheck.Timeout.String())
		if err != nil {
			return kobject.HealthCheck{}, errors.Wrap(err, "unable to parse health check timeout variable")
		}
		timeout = int32(parse.Seconds())
	}

	if composeHealthCheck.Interval != nil {
		parse, err := time.ParseDuration(composeHealthCheck.Interval.String())
		if err != nil {
			return kobject.HealthCheck{}, errors.Wrap(err, "unable to parse health check interval variable")
		}
		interval = int32(parse.Seconds())
	}

	if composeHealthCheck.Retries != nil {
		retries = int32(*composeHealthCheck.Retries)
	}

	if composeHealthCheck.StartPeriod != nil {
		parse, err := time.ParseDuration(composeHealthCheck.StartPeriod.String())
		if err != nil {
			return kobject.HealthCheck{}, errors.Wrap(err, "unable to parse health check startPeriod variable")
		}
		startPeriod = int32(parse.Seconds())
	}

	if composeHealthCheck.Test != nil {
		test = composeHealthCheck.Test[1:]
	}

	for key, value := range labels {
		switch key {
		case HealthCheckLivenessHTTPGetPath:
			httpPath = value
		case HealthCheckLivenessHTTPGetPort:
			httpPort = cast.ToInt32(value)
		}
	}

	// Due to docker/cli adding "CMD-SHELL" to the struct, we remove the first element of composeHealthCheck.Test
	return kobject.HealthCheck{
		Test:        test,
		HTTPPath:    httpPath,
		HTTPPort:    httpPort,
		Timeout:     timeout,
		Interval:    interval,
		Retries:     retries,
		StartPeriod: startPeriod,
	}, nil
}

func dockerComposeToKomposeMapping(project *compose.Project) (kobject.KomposeObject, error) {
	// Step 1. Initialize what's going to be returned
	komposeObject := kobject.KomposeObject{
		ServiceConfigs: make(map[string]kobject.ServiceConfig),
		LoadedFrom:     "compose",
		Secrets:        project.Secrets,
	}

	// Step 2. Parse through the object and convert it to kobject.KomposeObject!
	// Here we "clean up" the service configuration so we return something that includes
	// all relevant information as well as avoid the unsupported keys as well.
	for _, composeServiceConfig := range project.Services {
		// Standard import
		// No need to modify before importation
		name := composeServiceConfig.Name
		serviceConfig := kobject.ServiceConfig{}
		serviceConfig.Name = name
		serviceConfig.Image = composeServiceConfig.Image
		serviceConfig.WorkingDir = composeServiceConfig.WorkingDir
		serviceConfig.Annotations = composeServiceConfig.Labels
		serviceConfig.CapAdd = composeServiceConfig.CapAdd
		serviceConfig.CapDrop = composeServiceConfig.CapDrop
		serviceConfig.Expose = composeServiceConfig.Expose
		serviceConfig.Privileged = composeServiceConfig.Privileged
		serviceConfig.User = composeServiceConfig.User
		serviceConfig.Stdin = composeServiceConfig.StdinOpen
		serviceConfig.Tty = composeServiceConfig.Tty
		serviceConfig.TmpFs = composeServiceConfig.Tmpfs
		serviceConfig.ContainerName = normalizeContainerNames(composeServiceConfig.ContainerName)
		serviceConfig.Command = composeServiceConfig.Entrypoint
		serviceConfig.Args = composeServiceConfig.Command
		serviceConfig.Labels = composeServiceConfig.Labels
		serviceConfig.HostName = composeServiceConfig.Hostname
		serviceConfig.DomainName = composeServiceConfig.DomainName
		serviceConfig.Secrets = composeServiceConfig.Secrets

		if composeServiceConfig.StopGracePeriod != nil {
			serviceConfig.StopGracePeriod = composeServiceConfig.StopGracePeriod.String()
		}

		parseV3Network(&composeServiceConfig, &serviceConfig, project)

		if err := parseV3Resources(&composeServiceConfig, &serviceConfig); err != nil {
			return kobject.KomposeObject{}, err
		}

		if composeServiceConfig.Deploy != nil {
			// Deploy keys
			// mode:
			serviceConfig.DeployMode = composeServiceConfig.Deploy.Mode
			// labels
			serviceConfig.DeployLabels = composeServiceConfig.Deploy.Labels
		}

		// HealthCheck Liveness
		if composeServiceConfig.HealthCheck != nil && !composeServiceConfig.HealthCheck.Disable {
			var err error
			serviceConfig.HealthChecks.Liveness, err = parseHealthCheck(*composeServiceConfig.HealthCheck, *&composeServiceConfig.Labels)
			if err != nil {
				return kobject.KomposeObject{}, errors.Wrap(err, "Unable to parse health check")
			}
		}

		// HealthCheck Readiness
		var readiness, errReadiness = parseHealthCheckReadiness(*&composeServiceConfig.Labels)
		if readiness.Test != nil && len(readiness.Test) > 0 && len(readiness.Test[0]) > 0 && !readiness.Disable {
			serviceConfig.HealthChecks.Readiness = readiness
			if errReadiness != nil {
				return kobject.KomposeObject{}, errors.Wrap(errReadiness, "Unable to parse health check")
			}
		}

		// restart-policy: deploy.restart_policy.condition will rewrite restart option
		// see: https://docs.docker.com/compose/compose-file/#restart_policy
		serviceConfig.Restart = composeServiceConfig.Restart
		if composeServiceConfig.Deploy != nil && composeServiceConfig.Deploy.RestartPolicy != nil {
			serviceConfig.Restart = composeServiceConfig.Deploy.RestartPolicy.Condition
		}
		if serviceConfig.Restart == "unless-stopped" {
			log.Warnf("Restart policy 'unless-stopped' in service %s is not supported, convert it to 'always'", name)
			serviceConfig.Restart = "always"
		}

		// replicas:
		if composeServiceConfig.Deploy != nil && composeServiceConfig.Deploy.Replicas != nil {
			serviceConfig.Replicas = int(*composeServiceConfig.Deploy.Replicas)
		}

		// placement:
		if composeServiceConfig.Deploy != nil {
			serviceConfig.Placement = loadV3Placement(composeServiceConfig.Deploy.Placement)
		}

		if composeServiceConfig.Deploy != nil && composeServiceConfig.Deploy.UpdateConfig != nil {
			serviceConfig.DeployUpdateConfig = *composeServiceConfig.Deploy.UpdateConfig
		}

		// TODO: Build is not yet supported, see:
		// https://github.com/docker/cli/blob/master/cli/compose/types/types.go#L9
		// We will have to *manually* add this / parse.
		if composeServiceConfig.Build != nil {
			serviceConfig.Build = resolveV3Context(project.WorkingDir, composeServiceConfig.Build.Context)
			serviceConfig.Dockerfile = composeServiceConfig.Build.Dockerfile
			serviceConfig.BuildArgs = composeServiceConfig.Build.Args
			serviceConfig.BuildLabels = composeServiceConfig.Build.Labels
		}

		// env
		parseV3Environment(&composeServiceConfig, &serviceConfig)

		// Get env_file
		serviceConfig.EnvFile = composeServiceConfig.EnvFile

		// Parse the ports
		// v3 uses a new format called "long syntax" starting in 3.2
		// https://docs.docker.com/compose/compose-file/#ports

		// here we will translate `expose` too, they basically means the same thing in kubernetes
		serviceConfig.Port = loadV3Ports(composeServiceConfig.Ports, serviceConfig.Expose)

		// Parse the volumes
		// Again, in v3, we use the "long syntax" for volumes in terms of parsing
		// https://docs.docker.com/compose/compose-file/#long-syntax-3
		serviceConfig.VolList = loadV3Volumes(composeServiceConfig.Volumes)

		if err := parseKomposeLabels(composeServiceConfig.Labels, &serviceConfig); err != nil {
			return kobject.KomposeObject{}, err
		}

		// Log if the name will been changed
		if normalizeServiceNames(name) != name {
			log.Infof("Service name in docker-compose has been changed from %q to %q", name, normalizeServiceNames(name))
		}

		serviceConfig.Configs = composeServiceConfig.Configs
		serviceConfig.ConfigsMetaData = project.Configs
		if composeServiceConfig.Deploy != nil && composeServiceConfig.Deploy.EndpointMode == "vip" {
			serviceConfig.ServiceType = string(api.ServiceTypeNodePort)
		}
		// Final step, add to the array!
		komposeObject.ServiceConfigs[normalizeServiceNames(name)] = serviceConfig
	}

	handleV3Volume(&komposeObject, &project.Volumes)

	return komposeObject, nil
}

// resolveV3Context resolves build context
func resolveV3Context(wd string, context string) string {
	if context == "" {
		return ""
	}

	if context != "." {
		context = path.Join(wd, context)
	}

	return context
}

func parseV3Network(composeServiceConfig *compose.ServiceConfig, serviceConfig *kobject.ServiceConfig, project *compose.Project) {
	if len(composeServiceConfig.Networks) == 0 {
		if defaultNetwork, ok := project.Networks["default"]; ok {
			serviceConfig.Network = append(serviceConfig.Network, defaultNetwork.Name)
		}
	} else {
		var alias = ""
		for key := range composeServiceConfig.Networks {
			alias = key
			netName := project.Networks[alias].Name
			// if Network Name Field is empty in the docker-compose definition
			// we will use the alias name defined in service config file
			if netName == "" {
				netName = alias
			}
			serviceConfig.Network = append(serviceConfig.Network, netName)
		}
	}
}

func parseV3Resources(composeServiceConfig *compose.ServiceConfig, serviceConfig *kobject.ServiceConfig) error {
	if composeServiceConfig.Deploy == nil {
		return nil
	}
	if composeServiceConfig.Deploy.Resources.Limits != nil || composeServiceConfig.Deploy.Resources.Reservations != nil {
		// memory:
		// cpu:
		// convert to k8s format, for example: 0.5 = 500m
		// See: https://kubernetes.io/docs/concepts/configuration/manage-compute-resources-container/
		// "The expression 0.1 is equivalent to the expression 100m, which can be read as “one hundred millicpu”."

		// Since Deploy.Resources.Limits does not initialize, we must check type Resources before continuing
		if composeServiceConfig.Deploy.Resources.Limits != nil {
			serviceConfig.MemLimit = int64(composeServiceConfig.Deploy.Resources.Limits.MemoryBytes)

			if composeServiceConfig.Deploy.Resources.Limits.NanoCPUs != "" {
				cpuLimit, err := strconv.ParseFloat(composeServiceConfig.Deploy.Resources.Limits.NanoCPUs, 64)
				if err != nil {
					return errors.Wrap(err, "Unable to convert cpu limits resources value")
				}
				serviceConfig.CPULimit = int64(cpuLimit * 1000)
			}
		}
		if composeServiceConfig.Deploy.Resources.Reservations != nil {
			serviceConfig.MemReservation = int64(composeServiceConfig.Deploy.Resources.Reservations.MemoryBytes)

			if composeServiceConfig.Deploy.Resources.Reservations.NanoCPUs != "" {
				cpuReservation, err := strconv.ParseFloat(composeServiceConfig.Deploy.Resources.Reservations.NanoCPUs, 64)
				if err != nil {
					return errors.Wrap(err, "Unable to convert cpu limits reservation value")
				}
				serviceConfig.CPUReservation = int64(cpuReservation * 1000)
			}
		}
	}
	return nil
}

func parseV3Environment(composeServiceConfig *compose.ServiceConfig, serviceConfig *kobject.ServiceConfig) {
	// Gather the environment values
	// DockerCompose uses map[string]*string while we use []string
	// So let's convert that using this hack
	// Note: unset env pick up the env value on host if exist
	for name, value := range composeServiceConfig.Environment {
		var env kobject.EnvVar
		if value != nil {
			env = kobject.EnvVar{Name: name, Value: *value}
		} else {
			result, ok := os.LookupEnv(name)
			if ok {
				env = kobject.EnvVar{Name: name, Value: result}
			} else {
				continue
			}
		}
		serviceConfig.Environment = append(serviceConfig.Environment, env)
	}
}

// parseKomposeLabels parse kompose labels, also do some validation
func parseKomposeLabels(labels map[string]string, serviceConfig *kobject.ServiceConfig) error {
	// Label handler
	// Labels used to influence conversion of kompose will be handled
	// from here for docker-compose. Each loader will have such handler.

	if serviceConfig.Labels == nil {
		serviceConfig.Labels = make(map[string]string)
	}

	for key, value := range labels {
		switch key {
		case LabelServiceType:
			serviceType, err := handleServiceType(value)
			if err != nil {
				return errors.Wrap(err, "handleServiceType failed")
			}

			serviceConfig.ServiceType = serviceType
		case LabelServiceExpose:
			serviceConfig.ExposeService = strings.Trim(strings.ToLower(value), " ,")
		case LabelNodePortPort:
			serviceConfig.NodePortPort = cast.ToInt32(value)
		case LabelServiceExposeTLSSecret:
			serviceConfig.ExposeServiceTLS = value
		case LabelImagePullSecret:
			serviceConfig.ImagePullSecret = value
		case LabelImagePullPolicy:
			serviceConfig.ImagePullPolicy = value
		default:
			serviceConfig.Labels[key] = value
		}
	}

	if serviceConfig.ExposeService == "" && serviceConfig.ExposeServiceTLS != "" {
		return errors.New("kompose.service.expose.tls-secret was specified without kompose.service.expose")
	}

	if serviceConfig.ServiceType != string(api.ServiceTypeNodePort) && serviceConfig.NodePortPort != 0 {
		return errors.New("kompose.service.type must be nodeport when assign node port value")
	}

	if len(serviceConfig.Port) > 1 && serviceConfig.NodePortPort != 0 {
		return errors.New("cannot set kompose.service.nodeport.port when service has multiple ports")
	}

	return nil
}

func handleV3Volume(komposeObject *kobject.KomposeObject, volumes *compose.Volumes) {
	for name := range komposeObject.ServiceConfigs {
		// retrieve volumes of service
		vols, err := retrieveVolume(name, *komposeObject)
		if err != nil {
			errors.Wrap(err, "could not retrieve vvolume")
		}
		for volName, vol := range vols {
			size, selector := getV3VolumeLabels(vol.VolumeName, volumes)
			if len(size) > 0 || len(selector) > 0 {
				// We can't assign value to struct field in map while iterating over it, so temporary variable `temp` is used here
				var temp = vols[volName]
				temp.PVCSize = size
				temp.SelectorValue = selector
				vols[volName] = temp
			}
		}
		// We can't assign value to struct field in map while iterating over it, so temporary variable `temp` is used here
		var temp = komposeObject.ServiceConfigs[name]
		temp.Volumes = vols
		komposeObject.ServiceConfigs[name] = temp
	}
}

func getV3VolumeLabels(name string, volumes *compose.Volumes) (string, string) {
	size, selector := "", ""

	if volume, ok := (*volumes)[name]; ok {
		for key, value := range volume.Labels {
			if key == "kompose.volume.size" {
				size = value
			} else if key == "kompose.volume.selector" {
				selector = value
			}
		}
	}

	return size, selector
}

func checkUnsupportedKeyForV3(project *compose.Project) []string {
	if project == nil {
		return []string{}
	}

	var keysFound []string

	for _, service := range project.Services {
		for _, tmpConfig := range service.Configs {
			if tmpConfig.GID != "" {
				keysFound = append(keysFound, "long syntax config gid")
			}
			if tmpConfig.UID != "" {
				keysFound = append(keysFound, "long syntax config uid")
			}
		}

		if service.CredentialSpec != nil {
			keysFound = append(keysFound, "credential_spec")
		}
	}

	for _, config := range project.Configs {
		if config.External.External {
			keysFound = append(keysFound, "external config")
		}
	}

	return keysFound
}
