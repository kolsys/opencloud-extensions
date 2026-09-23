import { defineConfig } from '@opencloud-eu/extension-sdk'

export default defineConfig({
  name: 'video-thumbnails',
  build: {
    // The service embeds the build and serves it, see internal/web.
    outDir: '../internal/web/dist',
    emptyOutDir: true
  }
})
