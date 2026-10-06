import type { Router } from 'vue-router'
import { createLocationSpaces } from '@opencloud-eu/web-pkg'

/**
 * homeToSpaces makes "/" open the list of the project spaces instead of the
 * personal space. The runtime adds its own "/" after the setup of the apps
 * (bootstrap.ts, announceDefaults) and the router takes the first of the
 * routes of the same rank, so the one added here wins. The redirect is
 * resolved before the guards run, so the auth guard keeps the spaces as the
 * target of the login as well.
 */
export const homeToSpaces = (router: Router): void => {
  router.addRoute({ path: '/', redirect: () => createLocationSpaces('files-spaces-projects') })
}
