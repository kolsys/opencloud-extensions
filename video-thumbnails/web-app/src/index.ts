import {
  defineWebApplication,
  usePreviewService,
  useAppsStore,
  useRouter,
  ProcessorType
} from '@opencloud-eu/web-pkg'
import type { ApplicationSetupOptions, LoadPreviewOptions } from '@opencloud-eu/web-pkg'

import { closeToFolder } from './closeToFolder'
import { homeToSpaces } from './homeToSpaces'

/**
 * The web asks for a preview only when the PROPFIND says the file has one,
 * and the platform says so for the types its own thumbnails service renders.
 * Videos are rendered by the extension instead, so the answer is given here.
 */
const isVideo = (mimeType?: string) => !!mimeType && mimeType.toLowerCase().startsWith('video/')

/**
 * A tile draws its preview into a 16:9 box with object-fit: cover
 * (ResourceTile.vue), which cuts the top and the bottom off a vertical
 * frame. The tiles ask for a square fit; a video asks for the box itself
 * instead, and the service fits the frame into it over a blurred copy rather
 * than cutting it.
 */
const TILE_ASPECT = 16 / 9

const forTile = (options: LoadPreviewOptions): LoadPreviewOptions => {
  const { processor, dimensions } = options
  if (processor !== ProcessorType.enum.fit || !dimensions || dimensions[0] !== dimensions[1]) {
    return options
  }
  const [width] = dimensions
  return {
    ...options,
    processor: ProcessorType.enum.thumbnail,
    dimensions: [width, Math.round(width / TILE_ASPECT)]
  }
}

const withPreview = (options: LoadPreviewOptions): LoadPreviewOptions => {
  const { resource } = options
  if (!isVideo(resource.mimeType)) {
    return options
  }
  const video = resource.hasPreview?.() ? resource : { ...resource, hasPreview: () => true }
  return forTile({ ...options, resource: video })
}

export default defineWebApplication({
  setup({ applicationConfig }: ApplicationSetupOptions) {
    const previewService = usePreviewService()
    const loadPreview = previewService.loadPreview.bind(previewService)
    previewService.loadPreview = (options, ...rest) => loadPreview(withPreview(options), ...rest)

    // off unless the config of the app says `closeToFolder: true`
    if (applicationConfig?.closeToFolder === true) {
      closeToFolder(useRouter(), useAppsStore())
    }
    // off unless the config of the app says `homeToSpaces: true`
    if (applicationConfig?.homeToSpaces === true) {
      homeToSpaces(useRouter())
    }

    return {
      appInfo: {
        id: 'video-thumbnails',
        name: 'Video thumbnails'
      }
    }
  }
})
