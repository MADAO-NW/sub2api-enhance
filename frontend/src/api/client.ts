import axios, { type InternalAxiosRequestConfig } from 'axios'
export const apiClient=axios.create({baseURL:'/enhance/api/v1',withCredentials:true,headers:{'X-Enhance-Request':'1'}})
// 仅在当前页面内存复用原版令牌，不写入任何浏览器存储。
let bootstrapToken: string | null = null
let reconnecting: Promise<unknown> | null = null
export function setBootstrapToken(token: string | null) { bootstrapToken = token }
export function reloadMenu() {
  try {
    if (window.parent !== window && window.parent.location.origin === location.origin) { window.parent.location.reload(); return }
  } catch { /* 跨域嵌入只能刷新当前页面。 */ }
  window.location.reload()
}
apiClient.interceptors.response.use(response=>{
  if(response.data&&typeof response.data==='object'&&'code' in response.data){if(response.data.code!==0)return Promise.reject(response.data);response.data=response.data.data}
  return response
},async error=>{
  const config = error.config as (InternalAxiosRequestConfig & { sessionRetried?: boolean }) | undefined
  if(error.response?.status===401 && config?.url?.startsWith('/admin/')) {
    // 只有认证中间件明确拒绝、业务尚未执行时，才允许重试原请求。
    if(error.response.data?.code==='enhance_session_expired' && bootstrapToken && !config.sessionRetried) {
      config.sessionRetried=true
      try {
        if(!reconnecting) reconnecting=apiClient.post('/auth/bootstrap',{token:bootstrapToken}).finally(()=>{reconnecting=null})
        await reconnecting
        return await apiClient.request(config)
      } catch (failure) {
        if ((failure as { status?: number }).status !== 401) return Promise.reject(failure)
        bootstrapToken=null
      }
    }
    window.dispatchEvent(new Event('enhance-login-required'))
    return Promise.reject({status:401,message:'登录状态需要重新验证，请点击页面上方“重新连接”。'})
  }
  return Promise.reject({...error.response?.data,status:error.response?.status,message:error.response?.data?.message || error.message})
})
