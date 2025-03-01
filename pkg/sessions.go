package exporter

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/apigateway"
	"github.com/aws/aws-sdk-go/service/apigateway/apigatewayiface"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/cloudwatch/cloudwatchiface"
	"github.com/aws/aws-sdk-go/service/databasemigrationservice"
	"github.com/aws/aws-sdk-go/service/databasemigrationservice/databasemigrationserviceiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	r "github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi/resourcegroupstaggingapiiface"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/aws/aws-sdk-go/service/sts/stsiface"
)

// SessionCache is an interface to a cache of sessions and clients for all the
// roles specified by the exporter. For jobs with many duplicate roles, this provides
// relief to the AWS API and prevents timeouts by excessive credential requesting.
type SessionCache interface {
	GetSTS(Role) stsiface.STSAPI
	GetCloudwatch(*string, Role) cloudwatchiface.CloudWatchAPI
	GetTagging(*string, Role) resourcegroupstaggingapiiface.ResourceGroupsTaggingAPIAPI
	GetASG(*string, Role) autoscalingiface.AutoScalingAPI
	GetEC2(*string, Role) ec2iface.EC2API
	GetDMS(*string, Role) databasemigrationserviceiface.DatabaseMigrationServiceAPI
	GetAPIGateway(*string, Role) apigatewayiface.APIGatewayAPI
	Refresh()
	Clear()
}

type sessionCache struct {
	stsRegion        string
	session          *session.Session
	endpointResolver endpoints.ResolverFunc
	stscache         map[Role]stsiface.STSAPI
	clients          map[Role]map[string]*clientCache
	cleared          bool
	refreshed        bool
	mu               sync.Mutex
	fips             bool
	logger           Logger
}

type clientCache struct {
	// if we know that this job is only used for static
	// then we don't have to construct as many cached connections
	// later on
	onlyStatic bool
	cloudwatch cloudwatchiface.CloudWatchAPI
	tagging    resourcegroupstaggingapiiface.ResourceGroupsTaggingAPIAPI
	asg        autoscalingiface.AutoScalingAPI
	ec2        ec2iface.EC2API
	dms        databasemigrationserviceiface.DatabaseMigrationServiceAPI
	apiGateway apigatewayiface.APIGatewayAPI
}

// NewSessionCache creates a new session cache to use when fetching data from
// AWS.
func NewSessionCache(config ScrapeConf, fips bool, logger Logger) SessionCache {
	stscache := map[Role]stsiface.STSAPI{}
	roleCache := map[Role]map[string]*clientCache{}

	for _, discoveryJob := range config.Discovery.Jobs {
		for _, role := range discoveryJob.Roles {
			if _, ok := stscache[role]; !ok {
				stscache[role] = nil
			}
			if _, ok := roleCache[role]; !ok {
				roleCache[role] = map[string]*clientCache{}
			}
			for _, region := range discoveryJob.Regions {
				roleCache[role][region] = &clientCache{}
			}
		}
	}

	for _, staticJob := range config.Static {
		for _, role := range staticJob.Roles {
			if _, ok := stscache[role]; !ok {
				stscache[role] = nil
			}

			if _, ok := roleCache[role]; !ok {
				roleCache[role] = map[string]*clientCache{}
			}

			for _, region := range staticJob.Regions {
				// Only write a new region in if the region does not exist
				if _, ok := roleCache[role][region]; !ok {
					roleCache[role][region] = &clientCache{
						onlyStatic: true,
					}
				}
			}
		}
	}

	endpointResolver := endpoints.DefaultResolver().EndpointFor

	endpointUrlOverride := os.Getenv("AWS_ENDPOINT_URL")
	if endpointUrlOverride != "" {
		// allow override of all endpoints for local testing
		endpointResolver = func(service, region string, optFns ...func(*endpoints.Options)) (endpoints.ResolvedEndpoint, error) {
			return endpoints.ResolvedEndpoint{
				URL: endpointUrlOverride,
			}, nil
		}
	}

	return &sessionCache{
		stsRegion:        config.StsRegion,
		session:          nil,
		endpointResolver: endpointResolver,
		stscache:         stscache,
		clients:          roleCache,
		fips:             fips,
		cleared:          false,
		refreshed:        false,
		logger:           logger,
	}
}

// Refresh and Clear help to avoid using lock primitives by asserting that
// there are no ongoing writes to the map.
func (s *sessionCache) Clear() {
	if s.cleared {
		return
	}

	for role := range s.stscache {
		s.stscache[role] = nil
	}

	for role, regions := range s.clients {
		for region := range regions {
			s.clients[role][region].cloudwatch = nil
			s.clients[role][region].tagging = nil
			s.clients[role][region].asg = nil
			s.clients[role][region].ec2 = nil
			s.clients[role][region].dms = nil
			s.clients[role][region].apiGateway = nil
		}
	}
	s.cleared = true
	s.refreshed = false
}

func (s *sessionCache) Refresh() {
	// TODO: make all the getter functions atomic pointer loads and sets
	if s.refreshed {
		return
	}

	// sessions really only need to be constructed once at runtime
	if s.session == nil {
		s.session = createAWSSession(s.endpointResolver, s.logger.IsDebugEnabled())
	}

	for role := range s.stscache {
		s.stscache[role] = createStsSession(s.session, role, s.stsRegion, s.fips, s.logger.IsDebugEnabled())
	}

	for role, regions := range s.clients {
		for region := range regions {
			// if the role is just used in static jobs, then we
			// can skip creating other sessions and potentially running
			// into permissions errors or taking up needless cycles
			s.clients[role][region].cloudwatch = createCloudwatchSession(s.session, &region, role, s.fips, s.logger.IsDebugEnabled())
			if s.clients[role][region].onlyStatic {
				continue
			}

			s.clients[role][region].tagging = createTagSession(s.session, &region, role, s.logger.IsDebugEnabled())
			s.clients[role][region].asg = createASGSession(s.session, &region, role, s.logger.IsDebugEnabled())
			s.clients[role][region].ec2 = createEC2Session(s.session, &region, role, s.fips, s.logger.IsDebugEnabled())
			s.clients[role][region].dms = createDMSSession(s.session, &region, role, s.fips, s.logger.IsDebugEnabled())
			s.clients[role][region].apiGateway = createAPIGatewaySession(s.session, &region, role, s.fips, s.logger.IsDebugEnabled())
		}
	}

	s.cleared = false
	s.refreshed = true
}

func (s *sessionCache) GetSTS(role Role) stsiface.STSAPI {
	// if we have not refreshed then we need to lock in case we are accessing concurrently
	if !s.refreshed {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if sess, ok := s.stscache[role]; ok && sess != nil {
		return sess
	}
	s.stscache[role] = createStsSession(s.session, role, s.stsRegion, s.fips, s.logger.IsDebugEnabled())
	return s.stscache[role]
}

func (s *sessionCache) GetCloudwatch(region *string, role Role) cloudwatchiface.CloudWatchAPI {
	// if we have not refreshed then we need to lock in case we are accessing concurrently
	if !s.refreshed {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if sess, ok := s.clients[role][*region]; ok && sess.cloudwatch != nil {
		return sess.cloudwatch
	}
	s.clients[role][*region].cloudwatch = createCloudwatchSession(s.session, region, role, s.fips, s.logger.IsDebugEnabled())
	return s.clients[role][*region].cloudwatch
}

func (s *sessionCache) GetTagging(region *string, role Role) resourcegroupstaggingapiiface.ResourceGroupsTaggingAPIAPI {
	// if we have not refreshed then we need to lock in case we are accessing concurrently
	if !s.refreshed {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if sess, ok := s.clients[role][*region]; ok && sess.tagging != nil {
		return sess.tagging
	}

	s.clients[role][*region].tagging = createTagSession(s.session, region, role, s.fips)
	return s.clients[role][*region].tagging
}

func (s *sessionCache) GetASG(region *string, role Role) autoscalingiface.AutoScalingAPI {
	// if we have not refreshed then we need to lock in case we are accessing concurrently
	if !s.refreshed {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if sess, ok := s.clients[role][*region]; ok && sess.asg != nil {
		return sess.asg
	}

	s.clients[role][*region].asg = createASGSession(s.session, region, role, s.fips)
	return s.clients[role][*region].asg
}

func (s *sessionCache) GetEC2(region *string, role Role) ec2iface.EC2API {
	// if we have not refreshed then we need to lock in case we are accessing concurrently
	if !s.refreshed {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if sess, ok := s.clients[role][*region]; ok && sess.ec2 != nil {
		return sess.ec2
	}

	s.clients[role][*region].ec2 = createEC2Session(s.session, region, role, s.fips, s.logger.IsDebugEnabled())
	return s.clients[role][*region].ec2
}

func (s *sessionCache) GetDMS(region *string, role Role) databasemigrationserviceiface.DatabaseMigrationServiceAPI {
	// if we have not refreshed then we need to lock in case we are accessing concurrently
	if !s.refreshed {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if sess, ok := s.clients[role][*region]; ok && sess.dms != nil {
		return sess.dms
	}

	s.clients[role][*region].dms = createDMSSession(s.session, region, role, s.fips, s.logger.IsDebugEnabled())
	return s.clients[role][*region].dms
}

func (s *sessionCache) GetAPIGateway(region *string, role Role) apigatewayiface.APIGatewayAPI {
	// if we have not refreshed then we need to lock in case we are accessing concurrently
	if !s.refreshed {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if sess, ok := s.clients[role][*region]; ok && sess.apiGateway != nil {
		return sess.apiGateway
	}

	s.clients[role][*region].apiGateway = createAPIGatewaySession(s.session, region, role, s.fips, s.logger.IsDebugEnabled())
	return s.clients[role][*region].apiGateway

}

func setExternalID(ID string) func(p *stscreds.AssumeRoleProvider) {
	return func(p *stscreds.AssumeRoleProvider) {
		if ID != "" {
			p.ExternalID = aws.String(ID)
		}
	}
}

func setSTSCreds(sess *session.Session, config *aws.Config, role Role) *aws.Config {
	if role.RoleArn != "" {
		config.Credentials = stscreds.NewCredentials(
			sess, role.RoleArn, setExternalID(role.ExternalID))
	}
	return config
}

func getAwsRetryer() aws.RequestRetryer {
	return client.DefaultRetryer{
		NumMaxRetries: 5,
		// MaxThrottleDelay and MinThrottleDelay used for throttle errors
		MaxThrottleDelay: 10 * time.Second,
		MinThrottleDelay: 1 * time.Second,
		// For other errors
		MaxRetryDelay: 3 * time.Second,
		MinRetryDelay: 1 * time.Second,
	}
}

func createAWSSession(resolver endpoints.ResolverFunc, isDebugEnabled bool) *session.Session {

	config := aws.Config{
		CredentialsChainVerboseErrors: aws.Bool(true),
		EndpointResolver:              resolver,
	}

	if isDebugEnabled {
		config.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
		Config:            config,
	}))
	return sess
}

func createStsSession(sess *session.Session, role Role, region string, fips bool, isDebugEnabled bool) *sts.STS {
	maxStsRetries := 5
	config := &aws.Config{MaxRetries: &maxStsRetries}

	if region != "" {
		config = config.WithRegion(region).WithSTSRegionalEndpoint(endpoints.RegionalSTSEndpoint)
	}

	if fips {
		// https://aws.amazon.com/compliance/fips/
		endpoint := fmt.Sprintf("https://sts-fips.%s.amazonaws.com", region)
		config.Endpoint = aws.String(endpoint)
	}

	if isDebugEnabled {
		config.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	return sts.New(sess, setSTSCreds(sess, config, role))
}

func createCloudwatchSession(sess *session.Session, region *string, role Role, fips bool, isDebugEnabled bool) *cloudwatch.CloudWatch {

	config := &aws.Config{Region: region, Retryer: getAwsRetryer()}

	if fips {
		// https://docs.aws.amazon.com/general/latest/gr/cw_region.html
		endpoint := fmt.Sprintf("https://monitoring-fips.%s.amazonaws.com", *region)
		config.Endpoint = aws.String(endpoint)
	}

	if isDebugEnabled {
		config.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	return cloudwatch.New(sess, setSTSCreds(sess, config, role))
}

func createTagSession(sess *session.Session, region *string, role Role, isDebugEnabled bool) *r.ResourceGroupsTaggingAPI {
	maxResourceGroupTaggingRetries := 5
	config := &aws.Config{
		Region:                        region,
		MaxRetries:                    &maxResourceGroupTaggingRetries,
		CredentialsChainVerboseErrors: aws.Bool(true),
	}

	if isDebugEnabled {
		config.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	return r.New(sess, setSTSCreds(sess, config, role))
}

func createASGSession(sess *session.Session, region *string, role Role, isDebugEnabled bool) autoscalingiface.AutoScalingAPI {
	maxAutoScalingAPIRetries := 5
	config := &aws.Config{Region: region, MaxRetries: &maxAutoScalingAPIRetries}

	if isDebugEnabled {
		config.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	return autoscaling.New(sess, setSTSCreds(sess, config, role))
}

func createEC2Session(sess *session.Session, region *string, role Role, fips bool, isDebugEnabled bool) ec2iface.EC2API {
	maxEC2APIRetries := 10
	config := &aws.Config{Region: region, MaxRetries: &maxEC2APIRetries}
	if fips {
		// https://docs.aws.amazon.com/general/latest/gr/ec2-service.html
		endpoint := fmt.Sprintf("https://ec2-fips.%s.amazonaws.com", *region)
		config.Endpoint = aws.String(endpoint)
	}

	if isDebugEnabled {
		config.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	return ec2.New(sess, setSTSCreds(sess, config, role))
}

func createDMSSession(sess *session.Session, region *string, role Role, fips bool, isDebugEnabled bool) databasemigrationserviceiface.DatabaseMigrationServiceAPI {
	maxDMSAPIRetries := 5
	config := &aws.Config{Region: region, MaxRetries: &maxDMSAPIRetries}
	if fips {
		// https://docs.aws.amazon.com/general/latest/gr/dms.html
		endpoint := fmt.Sprintf("https://dms-fips.%s.amazonaws.com", *region)
		config.Endpoint = aws.String(endpoint)
	}

	if isDebugEnabled {
		config.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	return databasemigrationservice.New(sess, setSTSCreds(sess, config, role))
}

func createAPIGatewaySession(sess *session.Session, region *string, role Role, fips bool, isDebugEnabled bool) apigatewayiface.APIGatewayAPI {
	maxAPIGatewayAPIRetries := 5
	config := &aws.Config{Region: region, MaxRetries: &maxAPIGatewayAPIRetries}
	if fips {
		// https://docs.aws.amazon.com/general/latest/gr/apigateway.html
		endpoint := fmt.Sprintf("https://apigateway-fips.%s.amazonaws.com", *region)
		config.Endpoint = aws.String(endpoint)
	}

	if isDebugEnabled {
		config.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	return apigateway.New(sess, setSTSCreds(sess, config, role))
}
