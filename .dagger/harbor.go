// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"context"
	"dagger/harbor-cli/internal/dagger"
	"fmt"
	"time"
)

const (
		// coreImage        = "registry.goharbor.io/harbor-next/harbor-core:" + harborImageTag

)

// startPostgres starts PostgreSQL container for Harbor
func (m *HarborCli) startPostgres(ctx context.Context) (*dagger.Service, error) {
	svc, err := dag.Container().
		From("postgres:13").
		WithEnvVariable("POSTGRES_USER", "postgres").
		WithEnvVariable("POSTGRES_PASSWORD", "password").
		WithEnvVariable("POSTGRES_DB", "registry").
		WithExposedPort(POSTGRES_PORT).
		AsService().
		WithHostname("postgres").
		Start(ctx)
	return svc, err
}

// startRedis starts Redis container for Harbor
func (m *HarborCli) startRedis(ctx context.Context) (*dagger.Service, error) {
	svc, err := dag.Container().
		From("redis:7-alpine").
		WithExposedPort(REDIS_PORT).
		AsService().
		WithHostname("redis").
		Start(ctx)
	return svc, err
}

// startCore starts Harbor Core service
func (m *HarborCli) startCore(ctx context.Context, postgres *dagger.Service, redis *dagger.Service) (*dagger.Service, error) {
	coreConfig := m.Source.File(HARBOR_CONFIG_PATH + "/core/app.conf")
	envFile := m.Source.File(HARBOR_CONFIG_PATH + "/core/env")
	runScript := m.Source.File(HARBOR_CONFIG_PATH + "/run_env.sh")
	privateKey := m.Source.File(HARBOR_CONFIG_PATH + "/core/private_key.pem")

	// Get script content and write it as executable
	scriptContent, err := runScript.Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read run script: %w", err)
	}

	ctr := dag.Container().
		From("goharbor/harbor-core:"+HARBOR_VERSION).
		WithServiceBinding("postgres", postgres).
		WithServiceBinding("redis", redis).
		WithMountedFile("/etc/core/app.conf", coreConfig).
		WithMountedFile("/etc/core/private_key.pem", privateKey).
		WithFile("/envFile", envFile).
		WithNewFile("/run_script", scriptContent, dagger.ContainerWithNewFileOpts{Permissions: 0755})

	// Debug: Try running Harbor Core directly to see error
	fmt.Println("🔍 Testing Harbor Core startup...")
	testOut, testErr := ctr.WithExec([]string{"/run_script", "/harbor/harbor_core"}, dagger.ContainerWithExecOpts{
		ExperimentalPrivilegedNesting: false,
	}).Sync(ctx)
	
	if testErr != nil {
		fmt.Printf("❌ Harbor Core test failed: %v\n", testErr)
		// Try to get stderr
		stderr, _ := testOut.Stderr(ctx)
		stdout, _ := testOut.Stdout(ctx)
		fmt.Printf("STDOUT: %s\nSTDERR: %s\n", stdout, stderr)
	}

	return ctr.
		WithExposedPort(HARBOR_CORE_PORT, dagger.ContainerWithExposedPortOpts{ExperimentalSkipHealthcheck: true}).
		WithEntrypoint([]string{"/run_script", "/harbor/harbor_core"}).
		AsService().
		WithHostname("core").
		Start(ctx)
}

// waitForCoreHealth waits for Harbor Core to be healthy
func (m *HarborCli) waitForCoreHealth(ctx context.Context) error {
	timeout := time.After(3 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	fmt.Println("⏳ Waiting for Harbor Core to be healthy...")

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for Harbor Core to be healthy")
		case <-ticker.C:
			out, err := dag.Container().
				From("curlimages/curl:latest").
				WithEnvVariable("CACHEBUSTER", time.Now().String()).
				WithExec([]string{"curl", "-sf", "http://core:8080/api/v2.0/health"}).
				Stdout(ctx)

			if err == nil {
				fmt.Printf("✅ Harbor Core is healthy! Response: %s\n", out)
				return nil
			}
			fmt.Printf("⏳ Harbor Core not ready yet, retrying...\n")
		}
	}
}

// SetupHarbor starts all required Harbor services (following harbor-satellite pattern)
func (m *HarborCli) SetupHarbor(ctx context.Context) (*dagger.Service, error) {
	fmt.Println("🚀 Setting up Harbor environment...")

	// Start PostgreSQL
	postgres, err := m.startPostgres(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to start postgres: %w", err)
	}
	fmt.Println("✅ PostgreSQL service started")

	// Start Redis
	redis, err := m.startRedis(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to start redis: %w", err)
	}
	fmt.Println("✅ Redis service started")

	// Start Harbor Core with service bindings
	core, err := m.startCore(ctx, postgres, redis)
	if err != nil {
		return nil, fmt.Errorf("failed to start core service: %w", err)
	}
	fmt.Println("✅ Harbor Core service started")

	// Wait for Core to be healthy
	if err := m.waitForCoreHealth(ctx); err != nil {
		return nil, err
	}

	return core, nil
}

// TestWithHarbor runs all tests against a local Harbor instance
func (m *HarborCli) TestWithHarbor(ctx context.Context) (string, error) {
	// Setup Harbor services
	_, err := m.SetupHarbor(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to setup Harbor: %w", err)
	}

	fmt.Println("🧪 Running tests against local Harbor...")

	// Run tests with Harbor environment
	test := dag.Container().
		From("golang:"+GO_VERSION+"-alpine").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("go-mod-"+GO_VERSION)).
		WithEnvVariable("GOMODCACHE", "/go/pkg/mod").
		WithMountedCache("/go/build-cache", dag.CacheVolume("go-build-"+GO_VERSION)).
		WithEnvVariable("GOCACHE", "/go/build-cache").
		WithMountedDirectory("/src", m.Source).
		WithWorkdir("/src").
		WithEnvVariable("TEST_HARBOR_URL", "core:8080").
		WithEnvVariable("TEST_HARBOR_USERNAME", HARBOR_ADMIN_USER).
		WithEnvVariable("TEST_HARBOR_PASSWORD", HARBOR_ADMIN_PASSWORD).
		WithExec([]string{"go", "test", "-v", "./..."})

	return test.Stdout(ctx)
}
