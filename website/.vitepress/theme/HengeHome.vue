<script setup>
import "@fontsource/atkinson-hyperlegible-next/400";
import "@fontsource/atkinson-hyperlegible-next/700";
import "@fontsource/big-shoulders-display/800";
import "@fontsource/big-shoulders-display/900";
import "./home.css";
import { onBeforeUnmount, onMounted, useTemplateRef } from "vue";

import { start } from "./henge.js";

const root = useTemplateRef("root");
let stop = () => {};
onMounted(() => (stop = start(root.value)));
onBeforeUnmount(() => stop());
</script>

<template>
  <div ref="root" class="kh">
    <header class="top">
      <div class="wrap">
        <a class="brand" href="/"><img src="/logo.svg" alt="" /><span>Kubehenge</span></a>
        <nav aria-label="Site">
          <a href="/docs/install">docs</a>
          <a class="opt" href="/docs/clusters">clusters</a>
          <a class="opt" href="/docs/architecture">architecture</a>
          <a class="opt" href="/docs/roadmap">roadmap</a>
        </nav>
        <span class="spacer"></span>
        <span class="ver"><b>alpha</b></span>
        <a class="src" href="https://github.com/kubehenge/kubehenge">source ↗</a>
      </div>
    </header>

    <main>
      <section class="hero" aria-labelledby="h-hero">
        <div class="wrap">
          <div class="hero-head">
            <h1 id="h-hero" class="display">Kubernetes<br />for <span>Proxmox VE</span></h1>
            <div class="lede">
              <p class="prose">
                Kubehenge raises production-ready Kubernetes clusters on Proxmox VE and keeps them standing.
              </p>
              <div class="win cmdbox" data-copy-scope>
                <div class="bar">
                  <span class="title">as root, in a Proxmox VE node's shell</span><span class="spacer"></span
                  ><button class="btn copy" type="button" data-copy=".cmd">copy</button>
                </div>
                <!-- One line: the box keeps white space, and the prompt's space must stay. -->
                <!-- prettier-ignore -->
                <div class="tbody"><span class="ps">#</span> <span class="cmd" data-text="curl -fsSLO https://github.com/kubehenge/kubehenge/releases/latest/download/install.sh &amp;&amp; sh install.sh">curl -fsSLO <wbr />https://github.com/<wbr />kubehenge/kubehenge/<wbr />releases/<wbr />latest/download/<wbr />install.sh &amp;&amp; sh install.sh</span><span class="cur" aria-hidden="true"></span></div>
              </div>
            </div>
          </div>

          <div class="henge">
            <ul class="ring" data-ring aria-label="The henge: one stone per cluster, sample clusters"></ul>
            <svg class="ley" data-ley aria-hidden="true"></svg>
            <div class="controls">
              <div class="acts">
                <button class="btn act" type="button" data-act="raise" disabled>
                  <span class="lb">raise another</span><code>kubectl apply -f web.yaml</code>
                </button>
                <button class="btn act" type="button" data-act="decorate" disabled>
                  <span class="lb">decorate one</span><code>headlamp: on, in dev</code>
                </button>
                <button class="btn act" type="button" data-act="connect" disabled>
                  <span class="lb">connect two</span><code>cluster mesh: ci ↔ dev</code>
                </button>
                <button class="btn act" type="button" data-act="topple" disabled>
                  <span class="lb">topple one</span><code>kubectl -n clusters delete cluster dev</code>
                </button>
              </div>
              <output class="said" data-said aria-live="polite"></output>
            </div>
          </div>
        </div>
      </section>

      <section class="sec does" aria-labelledby="h-does">
        <div class="wrap grid">
          <div class="points">
            <h2 id="h-does" class="display">What it does</h2>
            <ul>
              <li>
                <b>Raises clusters.</b> kubeadm, RKE2 or k3s, built by Cluster API from VM templates, from
                <b>Create cluster</b> in the interface or with <code>kubectl apply</code>. Each gets a Proxmox VE pool,
                user and API token of its own, an ingress controller on every node, and, if you like, addresses for its
                load-balanced Services from MetalLB or kube-vip.
              </li>
              <li>
                <b>Decorates them.</b> Add-ons per cluster, when it is created or later: Headlamp, a web interface
                inside the cluster; Rancher, with every other cluster registered in it; monitoring, pod security and
                Istio; backups with Velero to your S3 store, encrypted with the cluster's own keys; GitOps with Argo CD
                or Flux, syncing it from a Git repository; and Cilium Cluster Mesh, which joins clusters into one pod
                network.
              </li>
              <li>
                <b>Signs in with your Proxmox VE users.</b> Any realm Proxmox VE has, second factor included; Proxmox
                VE's own permissions on each cluster's pool decide who may change it.
              </li>
              <li>
                <b>Keeps them standing.</b> A node that stays not ready for five minutes is replaced. Kubernetes is
                upgraded one node at a time, by hand or inside a maintenance window, and Kubehenge upgrades itself,
                installing only signed releases.
              </li>
            </ul>
          </div>
          <div class="win browser" data-rise aria-label="The Kubehenge interface: its sign-in page">
            <div class="bar">
              <span class="burl"><b>https://10.20.0.40/</b></span>
            </div>
            <div class="ui u-signin">
              <div class="u-signin-panel">
                <p class="u-brand"><img src="/logo.svg" alt="" />Kubehenge</p>
                <p class="u-muted">Sign in with your Proxmox VE account.</p>
                <div class="u-field">
                  <span class="u-label">User name</span
                  ><span><span class="u-input">alice</span><span class="u-hint">or user@realm</span></span>
                </div>
                <div class="u-field">
                  <span class="u-label">Password</span><span class="u-input">••••••••••••</span>
                </div>
                <div class="u-field">
                  <span class="u-label">Realm</span
                  ><span class="u-select"><span>Proxmox VE authentication server (pve)</span></span>
                </div>
                <div class="u-actions"><button type="button" class="primary" tabindex="-1">Sign in</button></div>
              </div>
            </div>
          </div>
        </div>
      </section>

      <section class="sec inst" aria-labelledby="h-inst">
        <div class="wrap grid">
          <div class="node">
            <h2 id="h-inst" class="display">One line, one container</h2>
            <ul class="lines">
              <li>
                One unprivileged Debian container on a Proxmox VE 9 node, and nothing else on the node unless you say
                yes to a Kubernetes button in its interface.
              </li>
              <li>
                Every download is checked against the checksums the script carries; the appliance's certificate is
                signed by the Proxmox VE cluster's own CA.
              </li>
              <li>
                Then, from the interface, an administrator sets up the management cluster: a few Talos Linux VMs running
                Cluster API and Kubehenge's controller.
              </li>
            </ul>
            <p class="readit">
              <a href="https://github.com/kubehenge/kubehenge/blob/main/appliance/install.sh"
                >read install.sh before you run it ↗</a
              >
            </p>
          </div>
          <nav class="links" aria-label="Documentation and source">
            <a href="/docs/install">Install</a>
            <a href="/docs/clusters">Clusters</a>
            <a href="/docs/architecture">Architecture</a>
            <a href="/docs/roadmap">Roadmap</a>
            <a href="https://github.com/kubehenge/kubehenge">Source ↗</a>
          </nav>
        </div>
        <div class="wrap">
          <p class="alpha">
            <b>Kubehenge is alpha.</b> Not everything on this page is built yet (the <a href="/docs/roadmap">roadmap</a>
            says what is), and any release may still change it completely, its settings, interface and ClusterClasses
            included. Do not run your production on it yet.
          </p>
        </div>
      </section>
    </main>

    <footer class="foot">
      <div class="wrap">
        <p>
          Apache-2.0. Kubehenge is an independent project and is not affiliated with or endorsed by Proxmox Server
          Solutions GmbH. Proxmox is a registered trademark of Proxmox Server Solutions GmbH.
        </p>
      </div>
    </footer>
  </div>
</template>
