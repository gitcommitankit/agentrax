# Cloud Workload Identity Guide (Azure AKS & AWS EKS)

Agentrax supports **keyless cloud identity** on Microsoft Azure and Amazon Web Services (AWS). This eliminates static API keys, passwords, and long-lived cloud credentials from Kubernetes `Secret` objects.

Instead, pods receive short-lived, auto-rotated OpenTelemetry/OIDC identity tokens projected directly into the container by the Kubernetes API server and exchanged with the cloud provider's STS/OAuth endpoints.

---

## 1. Microsoft Azure (AKS Workload Identity)

Azure Workload Identity uses the AKS cluster's OIDC issuer URL and Azure Entra ID (formerly Azure AD) federated identity credentials.

### Prerequisites & AKS Cluster Configuration

Ensure the AKS cluster has OIDC Issuer and Workload Identity enabled:

```bash
# Update existing cluster or include these flags on cluster creation:
az aks update \
  --resource-group <resource-group> \
  --name <cluster-name> \
  --enable-oidc-issuer \
  --enable-workload-identity

# Retrieve the cluster's OIDC Issuer URL:
AKS_OIDC_ISSUER=$(az aks show \
  --resource-group <resource-group> \
  --name <cluster-name> \
  --query "oidcIssuerProfile.issuerUrl" -o tsv)
```

### Step 1: Create Azure Managed Identity

```bash
az identity create \
  --name agentrax-operator-identity \
  --resource-group <resource-group>

IDENTITY_CLIENT_ID=$(az identity show \
  --name agentrax-operator-identity \
  --resource-group <resource-group> \
  --query "clientId" -o tsv)

TENANT_ID=$(az account show --query "tenantId" -o tsv)
```

### Step 2: Establish Federated Identity Credential

Link the Kubernetes ServiceAccount (`agentrax-controller-manager` in `agentrax-system`) to the Azure Managed Identity:

```bash
az identity federated-credential create \
  --name "agentrax-operator-federation" \
  --identity-name agentrax-operator-identity \
  --resource-group <resource-group> \
  --issuer "${AKS_OIDC_ISSUER}" \
  --subject "system:serviceaccount:agentrax-system:agentrax-controller-manager" \
  --audience "api://AzureADTokenExchange"
```

### Step 3: Deploy Agentrax via Helm

```bash
helm install agentrax agentrax/agentrax \
  --namespace agentrax-system \
  --create-namespace \
  --set workloadIdentity.provider=azure \
  --set workloadIdentity.enabled=true \
  --set workloadIdentity.azureClientId="${IDENTITY_CLIENT_ID}" \
  --set workloadIdentity.azureTenantId="${TENANT_ID}"
```

**What this configures:**

1. Annotates the ServiceAccount with:
   - `azure.workload.identity/client-id: <IDENTITY_CLIENT_ID>`
   - `azure.workload.identity/tenant-id: <TENANT_ID>`
2. Labels the operator Pod with `azure.workload.identity/use: "true"`.
3. The Azure mutating admission webhook injects projected tokens and sets `AZURE_FEDERATED_TOKEN_FILE` inside the container.

---

## 2. Amazon Web Services (AWS EKS IRSA)

AWS uses **IAM Roles for Service Accounts (IRSA)** to project signed OIDC tokens and exchange them with AWS Security Token Service (`sts:AssumeRoleWithWebIdentity`).

### Prerequisites & EKS Cluster Configuration

```bash
# Associate IAM OIDC provider with your EKS cluster:
eksctl utils associate-iam-oidc-provider \
  --cluster <cluster-name> \
  --approve
```

### Step 1: Create IAM Role with Trust Policy

Create a trust policy document (`trust.json`):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::<ACCOUNT_ID>:oidc-provider/<OIDC_PROVIDER>"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "<OIDC_PROVIDER>:sub": "system:serviceaccount:agentrax-system:agentrax-controller-manager",
          "<OIDC_PROVIDER>:aud": "sts.amazonaws.com"
        }
      }
    }
  ]
}
```

Create the IAM role and attach necessary policies:

```bash
aws iam create-role \
  --role-name AgentraxOperatorRole \
  --assume-role-policy-document file://trust.json

AWS_ROLE_ARN=$(aws iam get-role --role-name AgentraxOperatorRole --query "Role.Arn" -o tsv)
```

### Step 2: Deploy Agentrax via Helm or Kustomize

#### Via Helm:

```bash
helm install agentrax agentrax/agentrax \
  --namespace agentrax-system \
  --create-namespace \
  --set workloadIdentity.provider=aws \
  --set workloadIdentity.enabled=true \
  --set workloadIdentity.awsRoleArn="${AWS_ROLE_ARN}"
```

#### Via Kustomize Overlay:

Apply the overlay located at `config/workload-identity/`:

```bash
kubectl apply -k config/workload-identity
```

---

## 3. Verification & Troubleshooting

To verify that the workload identity token is successfully projected into the operator pod:

```bash
kubectl describe pod -n agentrax-system -l control-plane=controller-manager
```

Inspect the environment and mounted volumes:

- **Azure**: Look for `AZURE_FEDERATED_TOKEN_FILE=/var/run/secrets/azure/tokens/azure-identity-token` and volume `azure-identity-token`.
- **AWS**: Look for `AWS_WEB_IDENTITY_TOKEN_FILE=/var/run/secrets/eks.amazonaws.com/serviceaccount/token` and `AWS_ROLE_ARN`.
