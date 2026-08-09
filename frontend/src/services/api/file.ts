/**
 * Copyright 2025 RAIDS Lab
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
import { apiClient, apiDelete, apiGet, apiPost } from '@/services/client'
import { IResponse } from '@/services/types'

export interface FileItem {
  isdir: boolean
  modifytime: string
  name: string
  size: number
  sys?: never
}

export interface MoveFile {
  fileName: string
  dst: string
}

export const apiGetFiles = (path: string) =>
  apiGet<IResponse<FileItem[] | undefined>>(
    `ss/files/${encodeURIComponent(path.replace(/^\//, ''))}`
  )

export const apiGetRWFiles = (path: string) =>
  apiGet<IResponse<FileItem[] | undefined>>(`ss/rwfiles/${path.replace(/^\//, '')}`)

export const apiGetAdminFiles = (path: string) =>
  apiGet<IResponse<FileItem[] | undefined>>(`ss/admin/files/${path.replace(/^\//, '')}`)

export const apiMkdir = async (path: string) => {
  await apiClient('ss/' + path.replace(/^\//, ''), {
    method: 'MKCOL',
  })
}

export const apiFileDelete = (path: string) =>
  apiDelete<IResponse<string>>(`ss/delete/${path.replace(/^\//, '')}`)

export const apiMoveFile = (req: MoveFile, path: string) =>
  apiPost<IResponse<MoveFile>>(`ss/move/${path.replace(/^\//, '')}`, req)

export const apiGetDatasetFiles = (datasetID: number, path: string) =>
  apiGet<IResponse<FileItem[]>>(
    path === '' ? `ss/dataset/${datasetID}` : `ss/dataset/${datasetID}/${path.replace(/^\//, '')}`
  )

export interface DirectorySize {
  path: string
  size: number
  unit: string
  formatted: string
}

export const apiGetDirectorySize = (path: string) =>
  apiGet<IResponse<DirectorySize>>(`v1/storage/dirsize/${path.replace(/^\//, '')}`)

export interface MyQuota {
  space_quota: number
  space_quota_formatted: string
}

export const apiGetMyQuota = () => apiGet<IResponse<MyQuota>>('v1/storage/my-quota')

export interface StorageCapabilities {
  backend: string
  configured: boolean
  quota_enabled: boolean
  pvc_name: string
  pvc_namespace?: string
  pv_name?: string
  csi_driver?: string
  quota_provider: 'auto' | 'storageServer' | 'toolbox' | 'disabled'
  storage_server_available: boolean
  toolbox_available: boolean
  usage_readable: boolean
  quota_readable: boolean
  quota_writable: boolean
  reasons?: string[]
}

export const apiGetStorageCapabilities = () =>
  apiGet<IResponse<StorageCapabilities>>('v1/storage/capabilities')
