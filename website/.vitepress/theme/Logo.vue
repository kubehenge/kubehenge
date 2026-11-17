<script setup>
// Drawn inline rather than as an image: it sits beside the name, so a screen reader skips it, and
// crawlers that flag every empty alt find no image to flag. One source, shared with the interface.
import logo from "../../../web/src/logo.svg?raw";
</script>

<template>
  <span class="logo" aria-hidden="true" v-html="logo"></span>
</template>
