import priorityScheduling from './priorityScheduling'
import qualityOps from './qualityOps'
import accountOps from './accountOps'
import tokenGuard from './tokenGuard'
import pelicanTests from './pelicanTests'
import tokenGuardV2 from './tokenGuardV2'
import landing from './landing'
import common from './common'
import dashboard from './dashboard'
import channelMonitorV2 from './channelMonitorV2'
import supportTickets from './supportTickets'
import batchImage from './batchImage'
import admin from './admin'
import misc from './misc'

import requestTiming from './requestTiming'

import autoConfig from './autoConfig'
import channelOperations from './channelOperations'
import prismConfig from './prismConfig'

export default {
  autoConfig,
  channelOperations,
  prismConfig,
  priorityScheduling,
  qualityOps,
  accountOps,
  tokenGuard,
  pelicanTests,
  tokenGuardV2,
  requestTiming,
  ...landing,
  ...common,
  ...dashboard,
  ...channelMonitorV2,
  ...supportTickets,
  ...batchImage,
  admin,
  ...misc,
}
