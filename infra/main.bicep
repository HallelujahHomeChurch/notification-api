targetScope = 'resourceGroup'

param location string = resourceGroup().location
param containerAppEnvironmentName string = 'alive-env'
param containerRegistryName string = 'alive'
param keyVaultName string = 'alive-vault'
@minLength(1)
param migrationImage string
@minLength(1)
param runtimeImage string
param deployRuntime bool = true
param provisionPermissions bool = true
param serviceBusNamespaceName string = 'alive-notifications-${uniqueString(subscription().id, resourceGroup().id)}'
param queueName string = 'notifications-email'
param smtpAddr string
param smtpFrom string
param smtpAuthenticationEnabled bool = true
param smtpUsernameSecretName string = 'notification-smtp-username'
param smtpPasswordSecretName string = 'notification-smtp-password'

var acrPullRole = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '7f951dda-4ed3-4680-a7ca-43fe172d538d')
var serviceBusSenderRole = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '69a216fc-b8fb-44d8-bc22-1f3c2cd27a39')
var serviceBusReceiverRole = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4f6d3b9b-027b-4f4c-9142-0e5a2a2247e0')
var databaseSecretUrl = '${vault.properties.vaultUri}secrets/notification-database-url'
var encryptionSecretUrl = '${vault.properties.vaultUri}secrets/notification-data-encryption-key'
var hashSecretUrl = '${vault.properties.vaultUri}secrets/notification-hash-key'
var smtpUsernameSecretUrl = '${vault.properties.vaultUri}secrets/${smtpUsernameSecretName}'
var smtpPasswordSecretUrl = '${vault.properties.vaultUri}secrets/${smtpPasswordSecretName}'
var commonEnvironment = [
  { name: 'ENVIRONMENT', value: 'production' }
  { name: 'PORT', value: '8081' }
  { name: 'QUEUE_DRIVER', value: 'servicebus' }
  { name: 'SERVICEBUS_NAMESPACE', value: '${serviceBus.name}.servicebus.windows.net' }
  { name: 'SERVICEBUS_QUEUE_NAME', value: emailQueue.name }
  { name: 'SHUTDOWN_TIMEOUT_SECONDS', value: '30' }
]

resource environment 'Microsoft.App/managedEnvironments@2024-03-01' existing = {
  name: containerAppEnvironmentName
}

resource registry 'Microsoft.ContainerRegistry/registries@2023-07-01' existing = {
  name: containerRegistryName
}

resource vault 'Microsoft.KeyVault/vaults@2023-07-01' existing = {
  name: keyVaultName
}

resource serviceBus 'Microsoft.ServiceBus/namespaces@2024-01-01' = {
  name: serviceBusNamespaceName
  location: location
  sku: {
    name: 'Standard'
    tier: 'Standard'
  }
  properties: {
    minimumTlsVersion: '1.2'
    disableLocalAuth: true
    publicNetworkAccess: 'Enabled'
    zoneRedundant: false
  }
}

resource emailQueue 'Microsoft.ServiceBus/namespaces/queues@2024-01-01' = {
  parent: serviceBus
  name: queueName
  properties: {
    requiresDuplicateDetection: true
    duplicateDetectionHistoryTimeWindow: 'PT10M'
    lockDuration: 'PT2M'
    maxDeliveryCount: 10
    defaultMessageTimeToLive: 'P7D'
    deadLetteringOnMessageExpiration: true
    enablePartitioning: true
  }
}

resource apiIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'notification-api-identity'
  location: location
}

resource workerIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'notification-worker-identity'
  location: location
}

resource migrateIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'notification-migrate-identity'
  location: location
}

resource apiAcrPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (provisionPermissions) {
  name: guid(registry.id, apiIdentity.id, 'acr-pull')
  scope: registry
  properties: {
    principalId: apiIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: acrPullRole
  }
}

resource workerAcrPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (provisionPermissions) {
  name: guid(registry.id, workerIdentity.id, 'acr-pull')
  scope: registry
  properties: {
    principalId: workerIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: acrPullRole
  }
}

resource migrateAcrPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (provisionPermissions) {
  name: guid(registry.id, migrateIdentity.id, 'acr-pull')
  scope: registry
  properties: {
    principalId: migrateIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: acrPullRole
  }
}

resource apiServiceBusSender 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (provisionPermissions) {
  name: guid(emailQueue.id, apiIdentity.id, 'service-bus-sender')
  scope: emailQueue
  properties: {
    principalId: apiIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: serviceBusSenderRole
  }
}

resource workerServiceBusReceiver 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (provisionPermissions) {
  name: guid(emailQueue.id, workerIdentity.id, 'service-bus-receiver')
  scope: emailQueue
  properties: {
    principalId: workerIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: serviceBusReceiverRole
  }
}

// alive-vault currently uses access policies. Do not switch the shared vault to RBAC in this deployment.
resource notificationSecretAccess 'Microsoft.KeyVault/vaults/accessPolicies@2023-07-01' = if (provisionPermissions) {
  parent: vault
  name: 'add'
  properties: {
    accessPolicies: [
      {
        tenantId: subscription().tenantId
        objectId: apiIdentity.properties.principalId
        permissions: {
          secrets: ['get', 'list']
        }
      }
      {
        tenantId: subscription().tenantId
        objectId: workerIdentity.properties.principalId
        permissions: {
          secrets: ['get', 'list']
        }
      }
      {
        tenantId: subscription().tenantId
        objectId: migrateIdentity.properties.principalId
        permissions: {
          secrets: ['get', 'list']
        }
      }
    ]
  }
}

resource api 'Microsoft.App/containerApps@2025-01-01' = if (deployRuntime) {
  name: 'notification-api'
  location: location
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${apiIdentity.id}': {}
    }
  }
  properties: {
    managedEnvironmentId: environment.id
    configuration: {
      activeRevisionsMode: 'Single'
      dapr: {
        enabled: true
        appId: 'notification-api'
        appPort: 8081
        appProtocol: 'http'
        logLevel: 'warn'
      }
      registries: [
        {
          server: registry.properties.loginServer
          identity: apiIdentity.id
        }
      ]
      secrets: [
        { name: 'database-url', keyVaultUrl: databaseSecretUrl, identity: apiIdentity.id }
        { name: 'data-encryption-key', keyVaultUrl: encryptionSecretUrl, identity: apiIdentity.id }
        { name: 'hash-key', keyVaultUrl: hashSecretUrl, identity: apiIdentity.id }
      ]
    }
    template: {
      containers: [
        {
          name: 'notification-api'
          image: runtimeImage
          args: ['api']
          env: concat(commonEnvironment, [
            { name: 'AZURE_CLIENT_ID', value: apiIdentity.properties.clientId }
            { name: 'DATABASE_URL', secretRef: 'database-url' }
            { name: 'NOTIFICATION_DATA_ENCRYPTION_KEY', secretRef: 'data-encryption-key' }
            { name: 'NOTIFICATION_HASH_KEY', secretRef: 'hash-key' }
            { name: 'NOTIFICATION_ALLOWED_CALLERS', value: 'account-api,hhc-web-api' }
            { name: 'NOTIFICATION_ALLOW_DEV_CALLER_HEADER', value: 'false' }
          ])
          resources: {
            cpu: json('0.5')
            memory: '1Gi'
          }
          probes: [
            {
              type: 'Liveness'
              httpGet: { path: '/health', port: 8081 }
              initialDelaySeconds: 5
              periodSeconds: 30
            }
            {
              type: 'Readiness'
              httpGet: { path: '/ready', port: 8081 }
              initialDelaySeconds: 10
              periodSeconds: 10
            }
          ]
        }
      ]
      scale: {
        minReplicas: 1
        maxReplicas: 3
      }
    }
  }
  dependsOn: [
    apiAcrPull
    apiServiceBusSender
    notificationSecretAccess
  ]
}

resource worker 'Microsoft.App/containerApps@2025-01-01' = if (deployRuntime) {
  name: 'notification-worker'
  location: location
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${workerIdentity.id}': {}
    }
  }
  properties: {
    managedEnvironmentId: environment.id
    configuration: {
      activeRevisionsMode: 'Single'
      registries: [
        {
          server: registry.properties.loginServer
          identity: workerIdentity.id
        }
      ]
      secrets: concat([
        { name: 'database-url', keyVaultUrl: databaseSecretUrl, identity: workerIdentity.id }
        { name: 'data-encryption-key', keyVaultUrl: encryptionSecretUrl, identity: workerIdentity.id }
      ], smtpAuthenticationEnabled ? [
        { name: 'smtp-username', keyVaultUrl: smtpUsernameSecretUrl, identity: workerIdentity.id }
        { name: 'smtp-password', keyVaultUrl: smtpPasswordSecretUrl, identity: workerIdentity.id }
      ] : [])
    }
    template: {
      containers: [
        {
          name: 'notification-worker'
          image: runtimeImage
          args: ['worker']
          env: concat(commonEnvironment, [
            { name: 'AZURE_CLIENT_ID', value: workerIdentity.properties.clientId }
            { name: 'DATABASE_URL', secretRef: 'database-url' }
            { name: 'NOTIFICATION_DATA_ENCRYPTION_KEY', secretRef: 'data-encryption-key' }
            { name: 'SMTP_ADDR', value: smtpAddr }
            { name: 'SMTP_FROM', value: smtpFrom }
          ], smtpAuthenticationEnabled ? [
            { name: 'SMTP_USERNAME', secretRef: 'smtp-username' }
            { name: 'SMTP_PASSWORD', secretRef: 'smtp-password' }
          ] : [])
          resources: {
            cpu: json('0.5')
            memory: '1Gi'
          }
          probes: [
            {
              type: 'Liveness'
              httpGet: { path: '/health', port: 8081 }
              initialDelaySeconds: 5
              periodSeconds: 30
            }
            {
              type: 'Readiness'
              httpGet: { path: '/ready', port: 8081 }
              initialDelaySeconds: 10
              periodSeconds: 10
            }
          ]
        }
      ]
      scale: {
        minReplicas: 0
        maxReplicas: 5
        rules: [
          {
            name: 'service-bus-email'
            custom: {
              type: 'azure-servicebus'
              metadata: {
                namespace: serviceBus.name
                queueName: emailQueue.name
                messageCount: '5'
              }
              identity: workerIdentity.id
            }
          }
        ]
      }
    }
  }
  dependsOn: [
    workerAcrPull
    workerServiceBusReceiver
    notificationSecretAccess
  ]
}

resource migrate 'Microsoft.App/jobs@2024-03-01' = {
  name: 'notification-migrate'
  location: location
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${migrateIdentity.id}': {}
    }
  }
  properties: {
    environmentId: environment.id
    configuration: {
      triggerType: 'Manual'
      replicaTimeout: 300
      replicaRetryLimit: 1
      manualTriggerConfig: {
        parallelism: 1
        replicaCompletionCount: 1
      }
      registries: [
        {
          server: registry.properties.loginServer
          identity: migrateIdentity.id
        }
      ]
      secrets: [
        { name: 'database-url', keyVaultUrl: databaseSecretUrl, identity: migrateIdentity.id }
      ]
    }
    template: {
      containers: [
        {
          name: 'notification-migrate'
          image: migrationImage
          args: ['migrate']
          env: [
            { name: 'ENVIRONMENT', value: 'production' }
            { name: 'AZURE_CLIENT_ID', value: migrateIdentity.properties.clientId }
            { name: 'DATABASE_URL', secretRef: 'database-url' }
          ]
          resources: {
            cpu: json('0.25')
            memory: '0.5Gi'
          }
        }
      ]
    }
  }
  dependsOn: [
    migrateAcrPull
    notificationSecretAccess
  ]
}

output serviceBusNamespace string = serviceBus.name
output queue string = emailQueue.name
output apiName string = 'notification-api'
output workerName string = 'notification-worker'
output migrationJobName string = migrate.name
