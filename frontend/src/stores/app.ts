import {defineStore} from 'pinia'
import {ref} from 'vue'
export const useAppStore=defineStore('app',()=>{const message=ref(''),error=ref(false);function showError(value:string){message.value=value;error.value=true}function showSuccess(value:string){message.value=value;error.value=false}return {message,error,showError,showSuccess}})
