import {apiClient} from '@/api/client'
import type {AdminGroup} from '@/types'
export async function getAll(){return (await apiClient.get<AdminGroup[]>('/admin/groups')).data}
