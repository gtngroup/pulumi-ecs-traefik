package main

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi-aws/sdk/v5/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v5/go/aws/ecs"
	elb "github.com/pulumi/pulumi-aws/sdk/v5/go/aws/elasticloadbalancingv2"
	"github.com/pulumi/pulumi-aws/sdk/v5/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func toPulumiStringArray(a []string) pulumi.StringArrayInput {
	var res []pulumi.StringInput
	for _, s := range a {
		res = append(res, pulumi.String(s))
	}
	return pulumi.StringArray(res)
}

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {

		/* NETWORKING */
		vpc, subnet, err := getNetwork(ctx)
		if err != nil {
			return err
		}

		webSg, traefikSg, containerSg, err := createSecurityGroups(ctx, vpc)
		if err != nil {
			return err
		}

		/* ECS */
		cluster, err := createCluster(ctx)
		if err != nil {
			return err
		}

		/* IAM */
		ecsRole, traefikRole, err := createIAMRoles(ctx)
		if err != nil {
			return err
		}

		traefikPolicy, err := createPolicies(ctx)
		if err != nil {
			return err
		}

		// Policy Attachements
		_, err = iam.NewRolePolicyAttachment(ctx, "ecs-policy", &iam.RolePolicyAttachmentArgs{
			Role:      ecsRole.Name,
			PolicyArn: pulumi.String("arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"),
		})
		if err != nil {
			return err
		}

		_, err = iam.NewRolePolicyAttachment(ctx, "traefil-exec-policy", &iam.RolePolicyAttachmentArgs{
			Role:      traefikRole.Name,
			PolicyArn: traefikPolicy.Arn,
		})
		if err != nil {
			return err
		}

		/* LOAD BALANCING */

		// Create a load balancer to listen for HTTP traffic on port 80.
		webLb, err := elb.NewLoadBalancer(ctx, "web-lb", &elb.LoadBalancerArgs{
			Subnets:        toPulumiStringArray(subnet.Ids),
			SecurityGroups: pulumi.StringArray{webSg.ID().ToStringOutput()},
		})
		if err != nil {
			return err
		}

		// Target Groups

		traefikTg, traefikAPITg, err := createTargetGroups(ctx, vpc)
		if err != nil {
			return err
		}

		// Listeners
		err = createListeners(ctx, webLb, traefikTg, traefikAPITg)
		if err != nil {
			return err
		}

		//	Container Definitions

		whoamiContainerDef, traefikContainerDef := createContainerDefs(ctx, webLb, cluster)

		// Task Definitions

		whoamiTask, traefikTask, err := createTaskDefinitions(ctx, whoamiContainerDef, traefikContainerDef, ecsRole, traefikRole)
		if err != nil {
			return err
		}

		// Services

		err = createServices(ctx,
			subnet,                 // Neworking
			containerSg, traefikSg, // Security
			traefikTg, traefikAPITg, // Load Balancing
			cluster, whoamiTask, traefikTask, // ECS
		)
		if err != nil {
			return err
		}

		// Export the resulting web address.
		ctx.Export("url", webLb.DnsName)
		return nil
	})
}

// Read back the default VPC and public subnets, which we will use.
func getNetwork(ctx *pulumi.Context) (*ec2.LookupVpcResult, *ec2.GetSubnetIdsResult, error) {
	t := true
	vpc, err := ec2.LookupVpc(ctx, &ec2.LookupVpcArgs{Default: &t})
	if err != nil {
		return &ec2.LookupVpcResult{}, &ec2.GetSubnetIdsResult{}, err
	}
	subnet, err := ec2.GetSubnetIds(ctx, &ec2.GetSubnetIdsArgs{VpcId: vpc.Id})
	if err != nil {
		return &ec2.LookupVpcResult{}, &ec2.GetSubnetIdsResult{}, err
	}

	return vpc, subnet, nil
}

func createSecurityGroups(ctx *pulumi.Context, vpc *ec2.LookupVpcResult) (
	*ec2.SecurityGroup,
	*ec2.SecurityGroup,
	*ec2.SecurityGroup,
	error,
) {

	// Create a SecurityGroup that permits HTTP ingress and unrestricted egress.
	webSg, err := ec2.NewSecurityGroup(ctx, "web-sg", &ec2.SecurityGroupArgs{
		VpcId: pulumi.String(vpc.Id),
		Egress: ec2.SecurityGroupEgressArray{
			ec2.SecurityGroupEgressArgs{
				Protocol:   pulumi.String("-1"),
				FromPort:   pulumi.Int(0),
				ToPort:     pulumi.Int(0),
				CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
		},
		Ingress: ec2.SecurityGroupIngressArray{
			ec2.SecurityGroupIngressArgs{
				Protocol:   pulumi.String("tcp"),
				FromPort:   pulumi.Int(80),
				ToPort:     pulumi.Int(80),
				CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
			ec2.SecurityGroupIngressArgs{
				Protocol:   pulumi.String("tcp"),
				FromPort:   pulumi.Int(8080),
				ToPort:     pulumi.Int(8080),
				CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
		},
	})
	if err != nil {
		return nil, nil, nil, err
	}

	// allow traffic from ALB
	traefikSg, err := ec2.NewSecurityGroup(ctx, "traefik-sg", &ec2.SecurityGroupArgs{
		VpcId:       pulumi.String(vpc.Id),
		Description: pulumi.String("Allow http and https traffic from ALB"),
		Egress: ec2.SecurityGroupEgressArray{
			ec2.SecurityGroupEgressArgs{
				Protocol:   pulumi.String("-1"),
				FromPort:   pulumi.Int(0),
				ToPort:     pulumi.Int(0),
				CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
		},
		Ingress: ec2.SecurityGroupIngressArray{
			ec2.SecurityGroupIngressArgs{
				Protocol:       pulumi.String("tcp"),
				FromPort:       pulumi.Int(80),
				ToPort:         pulumi.Int(80),
				CidrBlocks:     pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				SecurityGroups: pulumi.StringArray{webSg.ID().ToStringOutput()},
			},
			ec2.SecurityGroupIngressArgs{
				Protocol:       pulumi.String("tcp"),
				FromPort:       pulumi.Int(8080),
				ToPort:         pulumi.Int(8080),
				CidrBlocks:     pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				SecurityGroups: pulumi.StringArray{webSg.ID().ToStringOutput()},
			},
		},
	})
	if err != nil {
		return nil, nil, nil, err
	}

	// allow traffic from Traefik
	containerSg, err := ec2.NewSecurityGroup(ctx, "container-sg", &ec2.SecurityGroupArgs{
		VpcId:       pulumi.String(vpc.Id),
		Description: pulumi.String("Allow traffic from traefik"),
		Egress: ec2.SecurityGroupEgressArray{
			ec2.SecurityGroupEgressArgs{
				Protocol:   pulumi.String("-1"),
				FromPort:   pulumi.Int(0),
				ToPort:     pulumi.Int(0),
				CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
		},
		Ingress: ec2.SecurityGroupIngressArray{
			ec2.SecurityGroupIngressArgs{
				Protocol:       pulumi.String("tcp"),
				FromPort:       pulumi.Int(80),
				ToPort:         pulumi.Int(80),
				CidrBlocks:     pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				SecurityGroups: pulumi.StringArray{traefikSg.ID().ToStringOutput()},
			},
		},
	})
	if err != nil {
		return nil, nil, nil, err
	}

	return webSg, traefikSg, containerSg, nil
}

func createCluster(ctx *pulumi.Context) (*ecs.Cluster, error) {
	// Create an ECS cluster to run a container-based service.
	return ecs.NewCluster(ctx, "traefik-cluster-demo", nil)
}

func createIAMRoles(ctx *pulumi.Context) (*iam.Role, *iam.Role, error) {
	// Create an IAM role that can be used by our service's task.
	ecsRole, err := iam.NewRole(ctx, "ecs-role", &iam.RoleArgs{
		AssumeRolePolicy: pulumi.String(`{
		"Version": "2008-10-17",
		"Statement": [{
			"Sid": "",
			"Effect": "Allow",
			"Principal": {
				"Service": "ecs-tasks.amazonaws.com"
			},
			"Action": "sts:AssumeRole"
		}]
	}`),
	})
	if err != nil {
		return nil, nil, err
	}

	// Create an IAM role that can be used by our service's task.
	traefikRole, err := iam.NewRole(ctx, "task-role", &iam.RoleArgs{
		Name: pulumi.String("traefik"),
		AssumeRolePolicy: pulumi.String(`{
		"Version": "2008-10-17",
		"Statement": [{
			"Sid": "",
			"Effect": "Allow",
			"Principal": {
				"Service": "ecs-tasks.amazonaws.com"
			},
			"Action": "sts:AssumeRole"
		}]
	}`),
	})
	if err != nil {
		return nil, nil, err
	}

	return ecsRole, traefikRole, nil
}

func createPolicies(ctx *pulumi.Context) (*iam.Policy, error) {
	return iam.NewPolicy(ctx, "TraefikECSPolicy", &iam.PolicyArgs{
		Name: pulumi.String("traefik_policy"),
		Policy: pulumi.String(`{
			"Version": "2012-10-17",
			"Statement": [
				{
					"Effect": "Allow",
					"Action": [
						"ecs:ListClusters",
						"ecs:DescribeClusters",
						"ecs:ListTasks",
						"ecs:DescribeTasks",
						"ecs:DescribeContainerInstances",
						"ecs:DescribeTaskDefinition",
						"ec2:DescribeInstances"
					],
					"Sid": "main",
					"Resource": "*"
				}
			]
		}`),
	})
}

func createTargetGroups(ctx *pulumi.Context, vpc *ec2.LookupVpcResult) (*elb.TargetGroup, *elb.TargetGroup, error) {
	traefikTg, err := elb.NewTargetGroup(ctx, "traefik-tg", &elb.TargetGroupArgs{
		Name:       pulumi.String("traefik"),
		Port:       pulumi.Int(80),
		Protocol:   pulumi.String("HTTP"),
		TargetType: pulumi.String("ip"),
		VpcId:      pulumi.String(vpc.Id),
		HealthCheck: elb.TargetGroupHealthCheckArgs{
			Path:    pulumi.String("/"),
			Matcher: pulumi.String("200-202,404"),
		},
	})
	if err != nil {
		return nil, nil, err
	}

	traefikAPITg, err := elb.NewTargetGroup(ctx, "traefikapi-tg", &elb.TargetGroupArgs{
		Name:       pulumi.String("traefikapi"),
		Port:       pulumi.Int(8080),
		Protocol:   pulumi.String("HTTP"),
		TargetType: pulumi.String("ip"),
		VpcId:      pulumi.String(vpc.Id),
		HealthCheck: elb.TargetGroupHealthCheckArgs{
			Path:    pulumi.String("/"),
			Matcher: pulumi.String("200-202,300-302"),
		},
	})
	if err != nil {
		return nil, nil, err
	}

	return traefikTg, traefikAPITg, nil
}

func createListeners(
	ctx *pulumi.Context,
	loadBalancer *elb.LoadBalancer,
	traefikTg *elb.TargetGroup,
	traefikAPITg *elb.TargetGroup,
) error {
	_, err := elb.NewListener(ctx, "traefik-listener", &elb.ListenerArgs{
		LoadBalancerArn: loadBalancer.Arn,
		Port:            pulumi.Int(80),
		DefaultActions: elb.ListenerDefaultActionArray{
			elb.ListenerDefaultActionArgs{
				Type:           pulumi.String("forward"),
				TargetGroupArn: traefikTg.Arn,
			},
		},
	})
	if err != nil {
		return err
	}

	_, err = elb.NewListener(ctx, "web-listener", &elb.ListenerArgs{
		LoadBalancerArn: loadBalancer.Arn,
		Port:            pulumi.Int(8080),
		DefaultActions: elb.ListenerDefaultActionArray{
			elb.ListenerDefaultActionArgs{
				Type:           pulumi.String("forward"),
				TargetGroupArn: traefikAPITg.Arn,
			},
		},
	})
	if err != nil {
		return err
	}

	return nil
}

func createContainerDefs(ctx *pulumi.Context, loadBalancer *elb.LoadBalancer, cluster *ecs.Cluster) (pulumi.StringOutput, pulumi.StringOutput) {
	whoamiContainerDef := loadBalancer.DnsName.ApplyT(func(dnsName string) (string, error) {
		def := `[{
				"name": "whoami",
				"image": "containous/whoami:v1.5.0",
				"portMappings": [{
					"containerPort": 80,
					"hostPort": 80,
					"protocol": "tcp"
				}],
				"dockerLabels": {
					"traefik.enable": "true",
					"traefik.http.routers.whoami.rule": "` + fmt.Sprintf("Host(`%s`)", dnsName) + `"
				}
			}]`
		return def, nil
	}).(pulumi.StringOutput)

	traefikContainerDef := cluster.Name.ApplyT(func(name string) (string, error) {
		fmtstr := `[{
			"name": "traefik",
			"image": "traefik:v2.7",
			"essential" : true,
			"entryPoint": ["traefik", "--providers.ecs.clusters", %q, "--log.level", "DEBUG", "--providers.ecs.region", "eu-central-1", "--api.insecure"],
			"portMappings": [
				{
					"containerPort": 80,
					"hostPort": 80,
					"protocol": "tcp"
				},
				{
					"containerPort": 8080,
					"hostPort": 8080,
					"protocol": "tcp"
				}
			],
			"Environment": [
				{
					"name": "AWS_ACCESS_KEY_ID",
					"value": %q
				}
			],
			"Secrets": [
				{
					"name": "AWS_SECRET_ACCESS_KEY",
					"valuefrom": %q
				}
			]
		}]`
		def := fmt.Sprintf(fmtstr, name, os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY_ARN"))
		return def, nil
	}).(pulumi.StringOutput)

	return whoamiContainerDef, traefikContainerDef
}

func createTaskDefinitions(
	ctx *pulumi.Context,
	whoamiContainerDef pulumi.StringOutput,
	traefikContainerDef pulumi.StringOutput,
	ecsRole *iam.Role,
	traefikRole *iam.Role,
) (*ecs.TaskDefinition, *ecs.TaskDefinition, error) {
	// whoami task
	whoamiTask, err := ecs.NewTaskDefinition(ctx, "app-task", &ecs.TaskDefinitionArgs{
		Family:                  pulumi.String("whoami"),
		ContainerDefinitions:    whoamiContainerDef,
		Cpu:                     pulumi.String("256"),
		Memory:                  pulumi.String("512"),
		NetworkMode:             pulumi.String("awsvpc"),
		RequiresCompatibilities: pulumi.StringArray{pulumi.String("FARGATE")},
		ExecutionRoleArn:        ecsRole.Arn,
	})
	if err != nil {
		return nil, nil, err
	}

	traefikTask, err := ecs.NewTaskDefinition(ctx, "traefik-task", &ecs.TaskDefinitionArgs{
		Family:                  pulumi.String("traefik"),
		ContainerDefinitions:    traefikContainerDef,
		Cpu:                     pulumi.String("256"),
		Memory:                  pulumi.String("512"),
		NetworkMode:             pulumi.String("awsvpc"),
		RequiresCompatibilities: pulumi.StringArray{pulumi.String("FARGATE")},
		ExecutionRoleArn:        ecsRole.Arn,
		TaskRoleArn:             traefikRole.Arn,
	})
	if err != nil {
		return nil, nil, err
	}

	return whoamiTask, traefikTask, nil
}

func createServices(
	ctx *pulumi.Context,
	subnet *ec2.GetSubnetIdsResult,
	containerSg *ec2.SecurityGroup,
	traefikSg *ec2.SecurityGroup,
	traefikTg *elb.TargetGroup,
	traefikAPITg *elb.TargetGroup,
	cluster *ecs.Cluster,
	whoamiTask *ecs.TaskDefinition,
	traefikTask *ecs.TaskDefinition,
) error {
	// whoami service
	_, err := ecs.NewService(ctx, "whoami-service", &ecs.ServiceArgs{
		Name: pulumi.String("whoami"),

		Cluster:        cluster.Arn,
		TaskDefinition: whoamiTask.Arn,

		DesiredCount: pulumi.Int(3),
		LaunchType:   pulumi.String("FARGATE"),

		NetworkConfiguration: &ecs.ServiceNetworkConfigurationArgs{
			AssignPublicIp: pulumi.Bool(true),
			Subnets:        toPulumiStringArray(subnet.Ids),
			SecurityGroups: pulumi.StringArray{containerSg.ID().ToStringOutput()},
		},
	})
	if err != nil {
		return err
	}

	// traefik service
	_, err = ecs.NewService(ctx, "traefik-service", &ecs.ServiceArgs{
		Name: pulumi.String("traefik"),

		Cluster:        cluster.Arn,
		TaskDefinition: traefikTask.Arn,

		DesiredCount: pulumi.Int(1),
		LaunchType:   pulumi.String("FARGATE"),

		LoadBalancers: ecs.ServiceLoadBalancerArray{
			ecs.ServiceLoadBalancerArgs{
				TargetGroupArn: traefikTg.Arn,
				ContainerName:  pulumi.String("traefik"),
				ContainerPort:  pulumi.Int(80),
			},
			ecs.ServiceLoadBalancerArgs{
				TargetGroupArn: traefikAPITg.Arn,
				ContainerName:  pulumi.String("traefik"),
				ContainerPort:  pulumi.Int(8080),
			},
		},

		NetworkConfiguration: &ecs.ServiceNetworkConfigurationArgs{
			AssignPublicIp: pulumi.Bool(true),
			Subnets:        toPulumiStringArray(subnet.Ids),
			SecurityGroups: pulumi.StringArray{traefikSg.ID().ToStringOutput()},
		},
	}, pulumi.DependsOn([]pulumi.Resource{traefikTg}))
	if err != nil {
		return err
	}

	return nil
}
