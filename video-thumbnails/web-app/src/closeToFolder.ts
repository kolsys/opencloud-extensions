import type { Router } from 'vue-router'
import { contextRouteNameKey } from '@opencloud-eu/web-pkg'
import type { ApplicationFileExtension } from '@opencloud-eu/web-pkg'

/** FileList is a location of the file list the client goes back to. */
export interface FileList {
  name: string
  params?: Record<string, string>
  query?: Record<string, string>
}

const one = (value: unknown): string | undefined => {
  const first = Array.isArray(value) ? value[0] : value
  return typeof first === 'string' && first !== '' ? first : undefined
}

/**
 * folderOf is where the file list would have sent the client back to from a
 * file opened in an app: the folder of the file, the shares list for a
 * shared file. It follows useLinkTargetRoute.getFileListLocation of the
 * platform and gives nothing where the file list could not resolve the
 * location.
 */
export const folderOf = (driveAliasAndItem: string, shareId?: string): FileList | undefined => {
  const item = driveAliasAndItem.replace(/\/+$/, '')
  const cut = item.lastIndexOf('/')
  if (cut < 0) {
    return undefined
  }
  const folder = item.slice(0, cut)
  const [kind] = folder.split('/')
  const isShare = kind === 'share' || kind === 'ocm-share'

  // <kind>/<name> is a space, not a file, unless the share is the file itself
  if (!folder.includes('/')) {
    return isShare ? { name: 'files-shares-with-me' } : undefined
  }
  if (kind === 'public' || kind === 'ocm') {
    return { name: 'files-public-link', params: { driveAliasAndItem: folder } }
  }
  // a share space is resolved by its id, the alias alone is not enough
  if (isShare && !shareId) {
    return undefined
  }
  return {
    name: 'files-spaces-generic',
    params: { driveAliasAndItem: folder },
    ...(shareId && { query: { shareId } })
  }
}

/**
 * closeToFolder makes an app opened by a link without a context query, the
 * plain /preview/<space>/<path> one, close into the folder of the file. The
 * client knows where to go back to only from the query the file list adds
 * when it opens the file; without it useAppNavigation.navigateToContext
 * pushes "/", the personal space. The guard takes that navigation to the
 * folder instead, for the routes the client opens files in.
 */
export const closeToFolder = (
  router: Router,
  appsStore: { fileExtensions: ApplicationFileExtension[] }
): void => {
  router.beforeEach((to, from) => {
    if (to.path !== '/' || from.query[contextRouteNameKey]) {
      return
    }
    const opensFiles = appsStore.fileExtensions.some(
      (extension) => (extension.routeName || extension.app) === from.name
    )
    const item = one(from.params.driveAliasAndItem)
    if (!opensFiles || !item) {
      return
    }

    const folder = folderOf(item, one(from.query.shareId))
    if (!folder) {
      return
    }
    const fileId = one(from.query.fileId)
    return {
      ...folder,
      query: { ...folder.query, ...(fileId && { scrollTo: fileId }) }
    }
  })
}
