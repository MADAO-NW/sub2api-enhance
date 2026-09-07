import axios from 'axios'
export const apiClient=axios.create({baseURL:'/enhance/api/v1',withCredentials:true,headers:{'X-Enhance-Request':'1'}})
apiClient.interceptors.response.use(response=>{if(response.data&&typeof response.data==='object'&&'code' in response.data){if(response.data.code!==0)return Promise.reject(response.data);response.data=response.data.data}return response},error=>Promise.reject(error.response?.data||error))
