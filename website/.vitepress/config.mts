import { defineConfig } from "vitepress";

const repository = "https://github.com/kubehenge/kubehenge";
const site = "https://kubehenge.dev";

export default defineConfig({
  title: "Kubehenge",
  description: "Kubernetes for Proxmox VE",
  lang: "en-GB",
  cleanUrls: true,
  lastUpdated: true,
  head: [
    ["link", { rel: "icon", type: "image/svg+xml", href: "/logo.svg" }],
    // What chat and social apps show for a shared link: Open Graph, and X's own card tags.
    ["meta", { property: "og:site_name", content: "Kubehenge" }],
    ["meta", { property: "og:type", content: "website" }],
    ["meta", { property: "og:locale", content: "en_GB" }],
    ["meta", { property: "og:image", content: `${site}/og.png` }],
    ["meta", { property: "og:image:width", content: "1200" }],
    ["meta", { property: "og:image:height", content: "630" }],
    ["meta", { property: "og:image:alt", content: "Kubehenge: Kubernetes for Proxmox VE, a ring of stones" }],
    ["meta", { name: "twitter:card", content: "summary_large_image" }],
  ],
  // Each page's own title, description and address, and the address search engines index it at.
  transformHead({ pageData, title, description }) {
    const path = pageData.relativePath.replace(/(?:^|\/)index\.md$/u, "").replace(/\.md$/u, "");
    const url = `${site}/${path}`;
    return [
      ["link", { rel: "canonical", href: url }],
      ["meta", { property: "og:url", content: url }],
      ["meta", { property: "og:title", content: title }],
      ["meta", { property: "og:description", content: description }],
    ];
  },
  sitemap: { hostname: site },
  themeConfig: {
    logo: { src: "/logo.svg", alt: "" },
    nav: [
      { text: "Docs", link: "/docs/install" },
      { text: "Changelog", link: "/docs/changelog" },
    ],
    sidebar: [
      {
        text: "Using Kubehenge",
        items: [
          { text: "Installing", link: "/docs/install" },
          { text: "Setup settings in depth", link: "/docs/install-settings" },
          { text: "Clusters", link: "/docs/clusters" },
          { text: "Upgrading", link: "/docs/upgrading" },
          { text: "Uninstalling", link: "/docs/uninstalling" },
          { text: "Troubleshooting", link: "/docs/troubleshooting" },
          { text: "VM templates", link: "/docs/templates" },
        ],
      },
      {
        text: "How it is made",
        items: [
          { text: "Architecture", link: "/docs/architecture" },
          { text: "Testing", link: "/docs/testing" },
          { text: "Releasing", link: "/docs/releasing" },
          { text: "Roadmap", link: "/docs/roadmap" },
          { text: "Changelog", link: "/docs/changelog" },
        ],
      },
    ],
    socialLinks: [{ icon: "github", link: repository }],
    editLink: { pattern: `${repository}/edit/main/:path`, text: "Change this page on GitHub" },
    search: { provider: "local" },
    footer: {
      message:
        "Apache-2.0. Not affiliated with or endorsed by Proxmox Server Solutions GmbH; Proxmox is its registered trademark.",
    },
  },
});
