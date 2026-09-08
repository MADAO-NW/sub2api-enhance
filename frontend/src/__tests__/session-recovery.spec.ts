import { AxiosError, AxiosHeaders } from 'axios'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { apiClient, setBootstrapToken } from '@/api/client'

beforeEach(()=>{setBootstrapToken('test-token')})
describe('session recovery',()=>{
 it('shares one bootstrap for concurrent expired requests and retries only once',async()=>{
  let bootstraps=0
  const attempts=new Map<string,number>()
  apiClient.defaults.adapter=async config=>{
   const response={config,headers:new AxiosHeaders(),status:200,statusText:'OK',data:{code:0,data:{ok:true}}}
   if(config.url==='/auth/bootstrap'){bootstraps++;await new Promise(r=>setTimeout(r,10));return response}
   const count=(attempts.get(config.url!)??0)+1;attempts.set(config.url!,count)
   if(count===1)throw new AxiosError('expired','401',config,undefined,{...response,status:401,data:{code:'enhance_session_expired'}})
   return response
  }
  await Promise.all([apiClient.get('/admin/a'),apiClient.post('/admin/b',{value:1})])
  expect(bootstraps).toBe(1);expect([...attempts.values()]).toEqual([2,2])
 })
 it('never replays a request rejected for upstream login or authorization failure',async()=>{
  let calls=0
  const event=vi.fn();window.addEventListener('enhance-login-required',event)
  apiClient.defaults.adapter=async config=>{calls++;throw new AxiosError('unauthorized','401',config,undefined,{config,headers:new AxiosHeaders(),status:401,statusText:'Unauthorized',data:{message:'原版权限失效'}})}
  await expect(apiClient.post('/admin/action',{})).rejects.toMatchObject({status:401})
  expect(calls).toBe(1);expect(event).toHaveBeenCalledTimes(1)
  window.removeEventListener('enhance-login-required',event)
 })
 it('stops after a failed bootstrap and asks for login',async()=>{
  let calls=0
  apiClient.defaults.adapter=async config=>{calls++;throw new AxiosError('unauthorized','401',config,undefined,{config,headers:new AxiosHeaders(),status:401,statusText:'Unauthorized',data:{code:'enhance_session_expired'}})}
  await expect(apiClient.get('/admin/a')).rejects.toMatchObject({status:401})
  expect(calls).toBe(2)
 })
})
