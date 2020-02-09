package request

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/sts"
)

type APIError struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Reason     string `json:"reason"`
	Code       int    `json:"code"`
}

// Do runs the given HTTP request.
func Do(method, url, body, certificateAuthorityData, clientCertificateData, clientKeyData, token, username, password string) (string, error) {
	var tlsConfig *tls.Config
	var err error

	if certificateAuthorityData != "" {
		tlsConfig, err = httpClientForRootCAs(certificateAuthorityData, clientCertificateData, clientKeyData)
		if err != nil {
			return "", err
		}
	}

	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
			Proxy:           http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

	req, err := http.NewRequest(method, url, bytes.NewBuffer([]byte(body)))
	if err != nil {
		return "", err
	}

	req.Header.Set("Accept", "application/json")

	if method == "PATCH" {
		req.Header.Set("Content-Type", "application/json-patch+json")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	if username != "" && password != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	if !(resp.StatusCode >= 200 && resp.StatusCode < 300) {
		var apiError APIError
		err := json.NewDecoder(resp.Body).Decode(&apiError)
		if err != nil {
			return "", fmt.Errorf(resp.Status)
		}

		return "", fmt.Errorf(apiError.Message)
	}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(respBody), nil
}

// httpClientForRootCAs return an HTTP client which trusts the provided root CAs.
func httpClientForRootCAs(certificateAuthorityData, clientCertificateData, clientKeyData string) (*tls.Config, error) {
	tlsConfig := tls.Config{RootCAs: x509.NewCertPool()}
	rootCA := []byte(certificateAuthorityData)

	if !tlsConfig.RootCAs.AppendCertsFromPEM(rootCA) {
		return nil, fmt.Errorf("no certs found in root CA file")
	}

	if clientCertificateData != "" && clientKeyData != "" {
		cert, err := tls.X509KeyPair([]byte(clientCertificateData), []byte(clientKeyData))
		if err != nil {
			return nil, err
		}

		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return &tlsConfig, nil
}

// AWSGetClusters returns all EKS clusters from AWS.
func AWSGetClusters(accessKeyId, secretAccessKey, region string) (string, error) {
	var clusters []*eks.Cluster
	var names []*string
	var nextToken *string

	cred := credentials.NewStaticCredentials(accessKeyId, secretAccessKey, "")

	sess, err := session.NewSession(&aws.Config{Region: aws.String(region), Credentials: cred})
	if err != nil {
		return "", err
	}

	eksClient := eks.New(sess)

	for {
		c, err := eksClient.ListClusters(&eks.ListClustersInput{NextToken: nextToken})
		if err != nil {
			return "", err
		}

		names = append(names, c.Clusters...)

		if c.NextToken == nil {
			break
		}

		nextToken = c.NextToken
	}

	for _, name := range names {
		cluster, err := eksClient.DescribeCluster(&eks.DescribeClusterInput{Name: name})
		if err != nil {
			return "", err
		}

		if *cluster.Cluster.Status == eks.ClusterStatusActive {
			clusters = append(clusters, cluster.Cluster)
		}
	}

	if clusters != nil {
		b, err := json.Marshal(clusters)
		if err != nil {
			return "", err
		}

		return string(b), nil
	}

	return "", nil
}

// AWSGetToken returns a bearer token for Kubernetes API requests.
// See: https://github.com/kubernetes-sigs/aws-iam-authenticator/blob/7547c74e660f8d34d9980f2c69aa008eed1f48d0/pkg/token/token.go#L310
func AWSGetToken(accessKeyId, secretAccessKey, region, clusterID string) (string, error) {
	cred := credentials.NewStaticCredentials(accessKeyId, secretAccessKey, "")

	sess, err := session.NewSession(&aws.Config{Region: aws.String(region), Credentials: cred})
	if err != nil {
		return "", err
	}

	stsClient := sts.New(sess)

	request, _ := stsClient.GetCallerIdentityRequest(&sts.GetCallerIdentityInput{})
	request.HTTPRequest.Header.Add("x-k8s-aws-id", clusterID)
	presignedURLString, err := request.Presign(60)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(`{"token": "k8s-aws-v1.%s"}`, base64.RawURLEncoding.EncodeToString([]byte(presignedURLString))), nil
}
