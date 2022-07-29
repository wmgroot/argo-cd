# Progressive Rollouts

This feature allows you to control the order that the ApplicationSet controller will use to create or update the Applications owned by an ApplicationSet resource.

## Use Cases
The Progressive Rollouts feature set is intended to be light and flexible. The feature is intended to only interact with the health of managed Applications. It is not intended to support direct integrations with other Rollout controllers (such as the native ReplicaSet controller or Argo Rollouts).

* Progressive Rollouts watch for the managed Application resources to become "Healthy" before proceeding to the next stage.
* Deployments, Daemonsets, StatefulSets, and [Argo Rollouts](https://argoproj.github.io/argo-rollouts/) are all supported, because the Application enters a "Progressing" state while pods are being rolled out.
* [Argo CD Resource Hooks](../../user-guide/resource_hooks.md) are supported. We recommend this approach for users that need advanced functionality when an Argo Rollout cannot be used, such as smoke testing after a Daemonset change.

## Strategies

* AllAtOnce (default)
* RollingUpdate

### AllAtOnce
This default Application update behavior is unchanged from the original ApplicationSet implementation.

All Applications managed by the ApplicationSet resource are updated simultaneously when the ApplicationSet is updated.

### RollingUpdate
This update strategy allows you to group Applications by labels present on the generated Application resources.
When the ApplicationSet changes, the changes will be applied to each group of Application resources sequentially.

* Application groups are selected by `matchExpressions`.
* All `matchExpressions` must be true for an Application to be selected (AND behavior).
* The `In` operator must match at least one value to be considered true (OR behavior).
* All Applications in each group must become Healthy before the ApplicationSet controller will proceed to update the next group of Applications.
* The number of simultaneous Application updates in a group will not exceed its `maxUpdate` parameter (0 or undefined means unbounded).

#### Example
The following example illustrates how to stage a progressive rollout over Applications with explicitly configured environment labels.
```
---
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: guestbook
spec:
  generators:
  - list:
      elements:
      - cluster: engineering-dev
        url: https://1.2.3.4
        env: dev
      - cluster: engineering-qa
        url: https://2.4.6.8
        env: qa
      - cluster: engineering-prod
        url: https://9.8.7.6/
        env: prod
  strategy:
    type: RollingUpdate
    rollingUpdate:
      steps:
        - matchExpressions:
            - key: env
              operator: In
              values:
                - dev
          maxUpdate: 0 # if undefined or 0, all applications matched are updated together
        - matchExpressions:
            - key: env
              operator: In
              values:
                - qa
        - matchExpressions:
            - key: env
              operator: In
              values:
                - prod
          maxUpdate: 1 # maxUpdate supports both integer and percentage string values
  template:
    metadata:
      name: '{{cluster}}-guestbook'
      labels:
        env: '{{env}}'
    spec:
      source:
        repoURL: https://github.com/infra-team/cluster-deployments.git
        targetRevision: HEAD
        path: guestbook/{{cluster}}
      destination:
        server: '{{url}}'
        namespace: guestbook
```

#### Limitations
The RollingUpdate strategy does not enforce sync order for external changes. Basically, if the ApplicationSet spec does not change, the RollingUpdate strategy has no mechanism available to control the sync order of the Applications it manages. The managed Applications will sync on their own (if autosync is enabled), and your desired rollingUpdate spec will be ignored.

This includes:

* Updates to manifests in a directory watched by the generated Application.
* Updates to an unversioned helm chart watched by the generated Application (including versioned upstream chart dependencies in the unversioned Chart.yaml).
