/*
Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License"). You may not use this file except in compliance with
the License. A copy of the License is located at

http://www.apache.org/licenses/LICENSE-2.0

or in the "license" file accompanying this file. This file is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the specific language governing permissions
and limitations under the License.
*/

package main

import (
	"encoding/base64"
	goErrors "errors"
	"fmt"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/private/protocol"
	"github.com/aws/aws-sdk-go/service/timestreamwrite"
	"github.com/go-kit/kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"timestream-prometheus-connector/errors"
	"timestream-prometheus-connector/timestream"
)

const (
	envName               = "IN_SUB_PROCESS"
	envValue              = "1"
	envString             = "IN_SUB_PROCESS=1"
	tableLabel            = "foo"
	databaseLabel         = "bar"
	assertInputMessage    = "Errors must not occur while marshalling input data."
	encodedBasicAuth      = "Basic QWxhZGRpbjpPcGVuU2VzYW1l"
	writeRequestType      = "*prompb.WriteRequest"
	awsCredentialsType    = "*credentials.Credentials"
)

var (
	oldArgs        = os.Args
	compareOptions = []cmp.Option{
		cmp.AllowUnexported(
			connectionConfig{},
			clientConfig{},
			promlog.AllowedFormat{},
			promlog.AllowedLevel{},
		),
		cmpopts.IgnoreFields(promlog.AllowedLevel{}, "o")}
	mockUnixTime    = time.Now().UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))
	validTimeSeries = &prompb.TimeSeries{
		Labels: []*prompb.Label{
			{
				Name:  model.MetricNameLabel,
				Value: "go_gc_duration_seconds",
			},
			{
				Name:  "label_1",
				Value: "value_1",
			},
			{
				Name:  databaseLabel,
				Value: "foo",
			},
			{
				Name:  tableLabel,
				Value: "bar",
			},
		},
		Samples: []prompb.Sample{
			{
				Timestamp: mockUnixTime,
				Value:     0.001995,
			},
		},
	}
	validWriteRequest = &prompb.WriteRequest{Timeseries: []*prompb.TimeSeries{validTimeSeries}}
	validWriteHeader  = map[string]string{"x-prometheus-remote-write-version": "0.1.0", basicAuthHeader: encodedBasicAuth}
)

type lambdaEnvOptions struct {
	key   string
	value string
}

type errReader int

// Read implements the io.Reader interface to return an error during read.
func (errReader) Read(p []byte) (n int, err error) {
    return 0, fmt.Errorf("error reading")
}

type mockWriter struct {
	mock.Mock
	writer
}

type requestTestCase struct {
	name               string
	lambdaOptions      []lambdaEnvOptions
	inputRequest       events.APIGatewayProxyRequest
	mockSDKError       error
	expectedStatusCode int
}

func (m *mockWriter) Write(req *prompb.WriteRequest, credentials *credentials.Credentials) ([3]int64, error) {
	resp := [3]int64{0, 0, 0}
	args := m.Called(req, credentials)
	return resp, args.Error(0)
}

// setUp returns a slice of valid arguments for the test and the expected configuration object after parseFlags().
func setUp() ([]string, *connectionConfig) {
	promLogFormat := &promlog.AllowedFormat{}
	promLogLevel := &promlog.AllowedLevel{}
	promLogFormat.Set("logfmt")
	promLogLevel.Set("info")

	return []string{"cmd", "--database-label=foo", "--table-label=bar"}, &connectionConfig{
		clientConfig:  &clientConfig{region: "us-east-1"},
		promlogConfig: promlog.Config{Format: promLogFormat, Level: promLogLevel},
		databaseLabel: "foo",
		tableLabel:    "bar",
		enableLogging: true,
		listenAddr:    ":9201",
		telemetryPath: "/metrics",
	}
}

// Resets the os.Args to the original value.
func cleanUp() {
	os.Args = oldArgs
}

func TestMainParseFlags(t *testing.T) {
	invalidFlagTestCases := []struct {
		testName string
		input    string
	}{
		{"error_from_invalid_label_flag", "--fail-on-long-label=2"},
		{"error_from_invalid_sample_flag", "--fail-on-invalid-sample=invalid"},
		{"error_from_invalid_enable_logging_flag", "--enable-logging=invalid"},
	}

	for _, test := range invalidFlagTestCases {
		t.Run(test.testName, func(t *testing.T) {
			if os.Getenv(envName) == envValue {
				args, _ := setUp()
				os.Args = append(args, test.input)
				parseFlags()
			}

			// Run the test in a subprocess.
			cmd := exec.Command(os.Args[0], fmt.Sprintf("-test.run=TestMainParseFlags/%s", test.testName))
			cmd.Env = append(os.Environ(), envString)
			err := cmd.Run()

			// Validate that an os.Exit error has occurred.
			e, ok := err.(*exec.ExitError)
			assert.True(t, ok, "Error is not an os.Exit(1) error")
			assert.False(t, e.Success(), "No errors were thrown by the program")

			cleanUp()
		})
	}

	t.Run("success parseFlags with default values", func(t *testing.T) {
		var expectedConfig *connectionConfig
		os.Args, expectedConfig = setUp()

		actualConfig := parseFlags()
		assert.NotNil(t, actualConfig)
		assert.True(
			t,
			cmp.Equal(expectedConfig, actualConfig, compareOptions...),
			"The actual configuration options parsed from flags do not match the expected configuration.",
		)

		cleanUp()
	})

	t.Run("error from missing required flags", func(t *testing.T) {
		if os.Getenv(envName) == envValue {
			parseFlags()
		}

		// Run the test in a subprocess.
		cmd := exec.Command(os.Args[0], "-test.run=TestMainParseFlags/error_from_missing_required_flags")
		cmd.Env = append(os.Environ(), envString)
		err := cmd.Run()

		// Validate that an os.Exit error has occurred.
		e, ok := err.(*exec.ExitError)
		assert.True(t, ok, "Error is not an os.Exit(1) error")
		assert.False(t, e.Success(), "No errors were thrown by the program")

		cleanUp()
	})
}

func TestParseBasicAuth(t *testing.T) {
	tests := []struct {
		name                string
		encodedCreds        string
		expectedCredentials *credentials.Credentials
		expectedAuthOk      bool
	}{
		{
			name:                "valid basic auth header",
			encodedCreds:        encodedBasicAuth,
			expectedCredentials: credentials.NewStaticCredentials("Aladdin", "OpenSesame", ""),
			expectedAuthOk:      true,
		},
		{
			name:                "empty basic auth header",
			encodedCreds:        "",
			expectedCredentials: nil,
			expectedAuthOk:      false,
		},
		{
			name:                "invalid basic auth header",
			encodedCreds:        "invalid",
			expectedCredentials: nil,
			expectedAuthOk:      false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			awsCredentials, authOk := parseBasicAuth(test.encodedCreds)
			assert.Equal(t, test.expectedAuthOk, authOk)
			assert.Equal(t, test.expectedCredentials, awsCredentials)
		})
	}

}

func TestLambdaHandlerPrepareRequest(t *testing.T) {
	validWriteRequestBody := prepareData(t)
	invalidSnappyEncodeRequestBody := make([]byte, base64.StdEncoding.EncodedLen(len([]byte("foo"))))
	base64.StdEncoding.Encode(invalidSnappyEncodeRequestBody, []byte("foo"))
	validBasicAuthHeader := make(map[string]string)
	validBasicAuthHeader[basicAuthHeader] = encodedBasicAuth
	invalidBasicAuthHeader := make(map[string]string)
	invalidBasicAuthHeader[basicAuthHeader] = "Basic "

	tests := []struct {
		name             string
		lambdaOptions    []lambdaEnvOptions
		inputRequest     events.APIGatewayProxyRequest
		expectedResponse events.APIGatewayProxyResponse
	}{
		{
			name:          "error no database and no table",
			lambdaOptions: []lambdaEnvOptions{},
			inputRequest: events.APIGatewayProxyRequest{
				IsBase64Encoded: true,
				Body:            string(validWriteRequestBody),
				Headers:         validBasicAuthHeader,
			},
			expectedResponse: events.APIGatewayProxyResponse{
				StatusCode: http.StatusBadRequest,
				Body:       errors.NewMissingDestinationError().(*errors.MissingDestinationError).Message()},
		},
		{
			name: "error decoding API Gateway request",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest: events.APIGatewayProxyRequest{
				IsBase64Encoded: true,
				Body:            "foo",
				Headers:         validBasicAuthHeader,
			},
			expectedResponse: events.APIGatewayProxyResponse{
				StatusCode: http.StatusBadRequest,
			},
		},
		{
			name: "error decoding Prometheus write request",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest: events.APIGatewayProxyRequest{
				IsBase64Encoded: true,
				Body:            string(invalidSnappyEncodeRequestBody),
				Headers:         validBasicAuthHeader,
			},
			expectedResponse: events.APIGatewayProxyResponse{
				StatusCode: http.StatusBadRequest,
			},
		},
		{
			name: "error no Prometheus remote request version header",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest: events.APIGatewayProxyRequest{
				IsBase64Encoded: true,
				Body:            string(validWriteRequestBody),
				Headers:         validBasicAuthHeader,
			},
			expectedResponse: events.APIGatewayProxyResponse{
				StatusCode: http.StatusBadRequest,
				Body:       errors.NewMissingHeaderError(writeHeader).(*errors.MissingHeaderError).Message()},
		},
		{
			name: "error no basic auth header",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest: events.APIGatewayProxyRequest{
				IsBase64Encoded: true,
				Body:            string(validWriteRequestBody),
			},
			expectedResponse: events.APIGatewayProxyResponse{
				StatusCode: http.StatusBadRequest,
				Body:       errors.NewParseBasicAuthHeaderError().(*errors.ParseBasicAuthHeaderError).Message()},
		},
		{
			name: "error invalid basic auth header",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest: events.APIGatewayProxyRequest{
				IsBase64Encoded: true,
				Body:            string(validWriteRequestBody),
				Headers:         invalidBasicAuthHeader,
			},
			expectedResponse: events.APIGatewayProxyResponse{
				StatusCode: http.StatusBadRequest,
				Body:       errors.NewParseBasicAuthHeaderError().(*errors.ParseBasicAuthHeaderError).Message()},
		},
		{
			name: "error parse environment variables",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
				{key: enableLogConfig.envFlag, value: "invalid"},
			},
			inputRequest: events.APIGatewayProxyRequest{
				IsBase64Encoded: true,
				Body:            string(validWriteRequestBody),
				Headers:         validBasicAuthHeader,
			},
			expectedResponse: events.APIGatewayProxyResponse{StatusCode: http.StatusBadRequest},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setEnvironmentVariables(test.lambdaOptions)

			actualResponse, _ := lambdaHandler(test.inputRequest)
			if len(test.expectedResponse.Body) == 0 {
				// Not a custom error from the connector, don't check check the error message.
				assert.Equal(t, http.StatusBadRequest, actualResponse.StatusCode)
			} else {
				assert.Equal(t, test.expectedResponse, actualResponse)
			}

			unsetEnvironmentVariables(test.lambdaOptions)
		})
	}
}

func TestLambdaHandlerWriteRequest(t *testing.T) {
	var emptyTimeSeries *prompb.TimeSeries
	validWriteRequestBody := prepareData(t)

	data, err := proto.Marshal(validTimeSeries)
	assert.Nil(t, err)

	invalidWriteRequest := encodeData(data)

	tests := []requestTestCase{
		{
			name: "success write request",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest:       events.APIGatewayProxyRequest{IsBase64Encoded: true, Body: string(validWriteRequestBody), Headers: validWriteHeader},
			mockSDKError:       nil,
			expectedStatusCode: http.StatusOK,
		},
		{
			name: "error unmarshalling write request",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest:       events.APIGatewayProxyRequest{IsBase64Encoded: true, Body: string(invalidWriteRequest), Headers: validWriteHeader},
			mockSDKError:       nil,
			expectedStatusCode: http.StatusBadRequest,
		},
		{
			name: "error during write",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest:       events.APIGatewayProxyRequest{IsBase64Encoded: true, Body: string(validWriteRequestBody), Headers: validWriteHeader},
			mockSDKError:       fmt.Errorf("foo"),
			expectedStatusCode: http.StatusBadRequest,
		},
		{
			name: "SDK error during write",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest:       events.APIGatewayProxyRequest{IsBase64Encoded: true, Body: string(validWriteRequestBody), Headers: validWriteHeader},
			mockSDKError:       &timestreamwrite.RejectedRecordsException{},
			expectedStatusCode: (&timestreamwrite.RejectedRecordsException{}).StatusCode(),
		},
		{
			name: "Missing database name from write",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest:       events.APIGatewayProxyRequest{IsBase64Encoded: true, Body: string(validWriteRequestBody), Headers: validWriteHeader},
			mockSDKError:       errors.NewMissingDatabaseWithWriteError(databaseLabel, emptyTimeSeries),
			expectedStatusCode: http.StatusBadRequest,
		},
		{
			name: "Missing table name from write",
			lambdaOptions: []lambdaEnvOptions{
				{key: tableLabelConfig.envFlag, value: tableLabel},
				{key: databaseLabelConfig.envFlag, value: databaseLabel},
			},
			inputRequest:       events.APIGatewayProxyRequest{IsBase64Encoded: true, Body: string(validWriteRequestBody), Headers: validWriteHeader},
			mockSDKError:       errors.NewMissingTableWithWriteError(tableLabel, emptyTimeSeries),
			expectedStatusCode: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mockTimestreamWriter := new(mockWriter)
			mockTimestreamWriter.On(
				"Write",
				mock.AnythingOfType(writeRequestType),
				mock.AnythingOfType(awsCredentialsType)).Return(test.mockSDKError)

			getWriteClient = func(timestreamClient *timestream.Client) writer {
				return mockTimestreamWriter
			}

			setEnvironmentVariables(test.lambdaOptions)

			res, _ := lambdaHandler(test.inputRequest)
			assert.Equal(t, test.expectedStatusCode, res.StatusCode)

			unsetEnvironmentVariables(test.lambdaOptions)
		})
	}
}

func TestCreateLogger(t *testing.T) {
	t.Run("success create no-op logger", func(t *testing.T) {
		nopLogger := log.NewNopLogger()
		config := &connectionConfig{}

		logger := config.createLogger()

		assert.Equal(t, nopLogger, logger)
	})

	t.Run("success create logger with config", func(t *testing.T) {
		nopLogger := log.NewNopLogger()

		promlogConfig := createDefaultPromlogConfig()
		config := &connectionConfig{enableLogging: true, promlogConfig: promlogConfig}

		logger := config.createLogger()
		assert.NotNil(t, logger)
		assert.NotEqual(t, nopLogger, logger, "Actual logger must not equal to log.NewNopLogger.")
	})
}

func TestBuildAWSConfig(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		expectedAWSConfig := &aws.Config{
			Region: aws.String("region"),
		}

		input := &connectionConfig{clientConfig: &clientConfig{region: "region"}}
		actualOutput := input.buildAWSConfig()

		assert.Equal(t, expectedAWSConfig, actualOutput)
	})
}

func TestParseEnvironmentVariables(t *testing.T) {
	defaultLogConfig := createDefaultPromlogConfig()

	tests := []struct {
		name           string
		lambdaOptions  []lambdaEnvOptions
		expectedConfig *connectionConfig
		expectedError  error
	}{
		{
			name:          "test default values",
			lambdaOptions: []lambdaEnvOptions{},
			expectedConfig: &connectionConfig{
				clientConfig:              &clientConfig{region: "us-east-1"},
				promlogConfig:             defaultLogConfig,
				enableLogging:             true,
				failOnInvalidSample:       false,
				failOnLongMetricLabelName: false,
			},
			expectedError: nil,
		},
		{
			name:           "error invalid enable_logging option",
			lambdaOptions:  []lambdaEnvOptions{{key: enableLogConfig.envFlag, value: "foo"}},
			expectedConfig: nil,
			expectedError:  errors.NewParseEnableLoggingError("foo"),
		},
		{
			name:           "error invalid fail_on_long_label option",
			lambdaOptions:  []lambdaEnvOptions{{key: failOnLabelConfig.envFlag, value: "foo"}},
			expectedConfig: nil,
			expectedError:  errors.NewParseMetricLabelError("foo"),
		},
		{
			name:           "error invalid fail_on_invalid_sample option",
			lambdaOptions:  []lambdaEnvOptions{{key: failOnInvalidSampleConfig.envFlag, value: "foo"}},
			expectedConfig: nil,
			expectedError:  errors.NewParseSampleOptionError("foo"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setEnvironmentVariables(test.lambdaOptions)

			config, err := parseEnvironmentVariables()
			assert.True(
				t,
				cmp.Equal(test.expectedConfig, config, compareOptions...),
				"The actual connectionConfig returned does not match the expected connectionConfig.")
			assert.Equal(t, test.expectedError, err)

			unsetEnvironmentVariables(test.lambdaOptions)
		})
	}
}

func TestWriteHandler(t *testing.T) {
	var emptyTimeSeries *prompb.TimeSeries
	tests := []struct {
		name                  string
		request               proto.Message
		returnError           error
		getWriteRequestReader func(t *testing.T, message proto.Message) io.Reader
		basicAuthHeader       string
		encodedBasicAuth      string
		expectedStatusCode    int
	}{
		{
			name:                  "success write",
			request:               validWriteRequest,
			returnError:           nil,
			getWriteRequestReader: getReaderHelper,
			basicAuthHeader:       basicAuthHeader,
			encodedBasicAuth:      encodedBasicAuth,
			expectedStatusCode:    http.StatusOK,
		},
		{
			name:                  "error decoding basic auth header",
			request:               validWriteRequest,
			returnError:           nil,
			getWriteRequestReader: getReaderHelper,
			basicAuthHeader:       basicAuthHeader,
			encodedBasicAuth:      "",
			expectedStatusCode:    http.StatusBadRequest,
		},
		{
			name:                  "error no basic auth header",
			request:               validWriteRequest,
			returnError:           nil,
			getWriteRequestReader: getReaderHelper,
			basicAuthHeader:       "",
			encodedBasicAuth:      "",
			expectedStatusCode:    http.StatusBadRequest,
		},
		{
			name:        "error reading request body",
			request:     nil,
			returnError: nil,
			getWriteRequestReader: func(t *testing.T, _ proto.Message) io.Reader {
				return errReader(0)
			},
			basicAuthHeader:    basicAuthHeader,
			encodedBasicAuth:   encodedBasicAuth,
			expectedStatusCode: http.StatusInternalServerError,
		},
		{
			name:        "error decoding",
			request:     nil,
			returnError: nil,
			getWriteRequestReader: func(t *testing.T, _ proto.Message) io.Reader {
				return strings.NewReader("foo")
			},
			basicAuthHeader:    basicAuthHeader,
			encodedBasicAuth:   encodedBasicAuth,
			expectedStatusCode: http.StatusBadRequest,
		},
		{
			name:                  "error unmarshalling request",
			request:               validTimeSeries,
			returnError:           nil,
			getWriteRequestReader: getReaderHelper,
			basicAuthHeader:       basicAuthHeader,
			encodedBasicAuth:      encodedBasicAuth,
			expectedStatusCode:    http.StatusBadRequest,
		},
		{
			name:    "SDK error from write",
			request: validWriteRequest,
			returnError: &timestreamwrite.RejectedRecordsException{
				RespMetadata: protocol.ResponseMetadata{StatusCode: 419},
			},
			getWriteRequestReader: getReaderHelper,
			basicAuthHeader:       basicAuthHeader,
			encodedBasicAuth:      encodedBasicAuth,
			expectedStatusCode:    419,
		},
		{
			name:                  "unknown SDK error from write",
			request:               validWriteRequest,
			returnError:           errors.NewSDKNonRequestError(goErrors.New("")),
			getWriteRequestReader: getReaderHelper,
			basicAuthHeader:       basicAuthHeader,
			encodedBasicAuth:      encodedBasicAuth,
			expectedStatusCode:    http.StatusBadRequest,
		},
		{
			name:                  "Missing database name from write",
			request:               validWriteRequest,
			returnError:           errors.NewMissingDatabaseWithWriteError(databaseLabel, emptyTimeSeries),
			getWriteRequestReader: getReaderHelper,
			basicAuthHeader:       basicAuthHeader,
			encodedBasicAuth:      encodedBasicAuth,
			expectedStatusCode:    http.StatusBadRequest,
		},
		{
			name:                  "Missing table name from write",
			request:               validWriteRequest,
			returnError:           errors.NewMissingTableWithWriteError(tableLabel, emptyTimeSeries),
			getWriteRequestReader: getReaderHelper,
			basicAuthHeader:       basicAuthHeader,
			encodedBasicAuth:      encodedBasicAuth,
			expectedStatusCode:    http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mockTimestreamWriter := new(mockWriter)
			mockTimestreamWriter.On(
				"Write",
				mock.AnythingOfType(writeRequestType),
				mock.AnythingOfType(awsCredentialsType)).Return(test.returnError)

			request, err := http.NewRequest("POST", "/write", test.getWriteRequestReader(t, test.request))
			assert.Nil(t, err)
			request.Header.Set(test.basicAuthHeader, test.encodedBasicAuth)

			logger := log.NewNopLogger()
			writers := []writer{mockTimestreamWriter}

			writeHandler := createWriteHandler(logger, writers)
			recorder := httptest.NewRecorder()
			handler := http.HandlerFunc(writeHandler)
			handler.ServeHTTP(recorder, request)

			resp := recorder.Result()

			assert.Equal(
				t,
				test.expectedStatusCode,
				resp.StatusCode,
				fmt.Sprintf("Expected status code %d, received %d", test.expectedStatusCode, resp.StatusCode))
		})
	}

	t.Run("long label name error from write", func(t *testing.T) {
		oldHalt := halt
		defer func() { halt = oldHalt }()
		got := 0
		mockHalt := func(code int) {
			got = code
		}
		halt = mockHalt

		mockTimestreamWriter := new(mockWriter)
		mockTimestreamWriter.On(
			"Write",
			mock.AnythingOfType(writeRequestType),
			mock.AnythingOfType(awsCredentialsType)).Return(errors.NewLongLabelNameError("", 0))
		getWriteRequestClient := func(t *testing.T) io.Reader {
			writeData, err := proto.Marshal(validWriteRequest)
			assert.Nil(t, err, assertInputMessage)
			return strings.NewReader(string(snappy.Encode(nil, writeData)))
		}
		request, err := http.NewRequest("POST", "/write", getWriteRequestClient(t))
		request.Header.Set(basicAuthHeader, encodedBasicAuth)
		assert.Nil(t, err)
		logger := log.NewNopLogger()
		writers := []writer{mockTimestreamWriter}
		writeHandler := createWriteHandler(logger, writers)
		recorder := httptest.NewRecorder()
		handler := http.HandlerFunc(writeHandler)
		handler.ServeHTTP(recorder, request)
		assert.Equal(t, 1, got)
	})
}

// prepareData marshals and encodes valid read and write requests for unit tests.
func prepareData(t *testing.T) ([]byte) {
	writeData, err := proto.Marshal(validWriteRequest)
	assert.Nil(t, err)

	return encodeData(writeData)

}

// encodeData encodes the data into snappy format then encodes the data using the standard base64 encoding.
func encodeData(data []byte) []byte {
	snappyEncodeData := snappy.Encode(nil, data)
	encodedData := make([]byte, base64.StdEncoding.EncodedLen(len(snappyEncodeData)))
	base64.StdEncoding.Encode(encodedData, snappyEncodeData)
	return encodedData
}

// setEnvironmentVariables sets the environment variables to the appropriate values.
func setEnvironmentVariables(options []lambdaEnvOptions) {
	for i := range options {
		option := options[i]
		os.Setenv(option.key, option.value)
	}
}

// unsetEnvironmentVariables clears the assigned Lambda environment options.
func unsetEnvironmentVariables(options []lambdaEnvOptions) {
	for i := range options {
		option := options[i]
		os.Unsetenv(option.key)
	}
}

// createDefaultPromlogConfig creates a promlog.Config with info debug level and logfmt debug format.
func createDefaultPromlogConfig() promlog.Config {
	format := &promlog.AllowedFormat{}
	level := &promlog.AllowedLevel{}
	format.Set("logfmt")
	level.Set("info")
	promlogConfig := promlog.Config{Level: level, Format: format}
	return promlogConfig
}

// getReaderHelper returns a reader for test.
func getReaderHelper(t *testing.T, message proto.Message) io.Reader {
	data, err := proto.Marshal(message)
	assert.Nil(t, err, assertInputMessage)
	return strings.NewReader(string(snappy.Encode(nil, data)))
}