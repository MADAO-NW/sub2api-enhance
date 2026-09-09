import {defineStore} from 'pinia'
import {ref} from 'vue'
export const useAppStore=defineStore('app',()=>{
  const message=ref(''),error=ref(false)
  let timer: ReturnType<typeof setTimeout>|undefined
  function dismiss(){if(timer)clearTimeout(timer);timer=undefined;message.value=''}
  function show(value:string,isError:boolean){dismiss();message.value=value;error.value=isError;timer=setTimeout(dismiss,3000)}
  function showError(value:string){show(value,true)}
  function showSuccess(value:string){show(value,false)}
  return {message,error,showError,showSuccess,dismiss}
})
