<!-- BEGIN MUNGE: UNVERSIONED_WARNING -->

<!-- BEGIN STRIP_FOR_RELEASE -->

<img src="http://kubernetes.io/img/warning.png" alt="WARNING"
     width="25" height="25">
<img src="http://kubernetes.io/img/warning.png" alt="WARNING"
     width="25" height="25">
<img src="http://kubernetes.io/img/warning.png" alt="WARNING"
     width="25" height="25">
<img src="http://kubernetes.io/img/warning.png" alt="WARNING"
     width="25" height="25">
<img src="http://kubernetes.io/img/warning.png" alt="WARNING"
     width="25" height="25">

<h2>PLEASE NOTE: This document applies to the HEAD of the source tree</h2>

If you are using a released version of Kubernetes, you should
refer to the docs that go with that version.

Documentation for other releases can be found at
[releases.k8s.io](http://releases.k8s.io).
</strong>
--

<!-- END STRIP_FOR_RELEASE -->

<!-- END MUNGE: UNVERSIONED_WARNING -->

# Replica Set

## What is a _replica set_?

Replica set is the next generation replication controller. The only difference between a _replica set_ and a _replication controller_ right now is the selector support. Replica set supports the new set-based selector requirements as described in the [labels user guide](labels.md#label-selectors) whereas a replication controller only supports equality-based selector requirements.

While replica sets can be used indepently, today it's mainly used by [deployments](deployments.md) as a mechanism to orchestrate pod creation, deletion and updates.


<!-- BEGIN MUNGE: GENERATED_ANALYTICS -->
[![Analytics](https://kubernetes-site.appspot.com/UA-36037335-10/GitHub/docs/user-guide/replicaset/README.md?pixel)]()
<!-- END MUNGE: GENERATED_ANALYTICS -->
