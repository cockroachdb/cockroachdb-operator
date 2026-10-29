//go:build !integration

package testutils

// IntegrationTestsEnabled should be checked to see if an integration test
// should be run. We want to always build integration tests, but only run them
// if they are enabled.
const IntegrationTestsEnabled = false
