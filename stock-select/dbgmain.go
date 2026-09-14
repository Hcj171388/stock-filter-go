package main
import ("fmt";"os";"stocklib")
func main(){
  d,err:=stocklib.FetchEM(map[string]string{"pn":"1","pz":"5","fid":"f12","po":"1","fs":"m:1+t:2","fields":"f12,f14,f2,f3,f8,f100,f62,f21","fltt":"2","invt":"2"})
  if err!=nil{fmt.Println("ERR",err);os.Exit(1)}
  fmt.Println("rows=",len(d.Data.Diff))
  for _,it:=range d.Data.Diff{
    for _,k:=range []string{"f12","f2","f3","f62","f21"}{
      if v,ok:=it[k];ok{fmt.Printf("%s => %T : %v\n",k,v,v)}else{fmt.Printf("%s => MISSING\n",k)}
    }
    fmt.Println("---")
  }
}
