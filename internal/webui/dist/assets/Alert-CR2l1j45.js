import{B as d,D as t,C as v,by as O,A as W,d as D,I as k,g as o,e as l,bz as K,O as N,Q as q,S as J,n as x,r as Q,c as h,J as i,$ as Z,K as f,a as G,Z as _,bA as U,V as s,a1 as X,bp as Y,bq as ee,bs as re,br as oe}from"./index-D2GZLkYI.js";import{b as te,r as ne,g as se}from"./use-message-C58DTaNH.js";var ae=d("alert",`
 line-height: var(--n-line-height);
 border-radius: var(--n-border-radius);
 position: relative;
 transition: background-color .3s var(--n-bezier);
 background-color: var(--n-color);
 text-align: start;
 word-break: break-word;
`,[t("border",`
 border-radius: inherit;
 position: absolute;
 left: 0;
 right: 0;
 top: 0;
 bottom: 0;
 transition: border-color .3s var(--n-bezier);
 border: var(--n-border);
 pointer-events: none;
 `),v("closable",[d("alert-body",[t("title",`
 padding-right: 24px;
 `)])]),t("icon",{color:"var(--n-icon-color)"}),d("alert-body",{padding:"var(--n-padding)"},[t("title",{color:"var(--n-title-text-color)"}),t("content",{color:"var(--n-content-text-color)"})]),O({originalTransition:"transform .3s var(--n-bezier)",enterToProps:{transform:"scale(1)"},leaveToProps:{transform:"scale(0.9)"}}),t("icon",`
 position: absolute;
 left: 0;
 top: 0;
 align-items: center;
 justify-content: center;
 display: flex;
 width: var(--n-icon-size);
 height: var(--n-icon-size);
 font-size: var(--n-icon-size);
 margin: var(--n-icon-margin);
 `),t("close",`
 transition:
 color .3s var(--n-bezier),
 background-color .3s var(--n-bezier);
 position: absolute;
 right: 0;
 top: 0;
 margin: var(--n-close-margin);
 `),v("show-icon",[d("alert-body",{paddingLeft:"calc(var(--n-icon-margin-left) + var(--n-icon-size) + var(--n-icon-margin-right))"})]),v("right-adjust",[d("alert-body",{paddingRight:"calc(var(--n-close-size) + var(--n-padding) + 2px)"})]),d("alert-body",`
 border-radius: var(--n-border-radius);
 transition: border-color .3s var(--n-bezier);
 `,[t("title",`
 transition: color .3s var(--n-bezier);
 font-size: 16px;
 line-height: 19px;
 font-weight: var(--n-title-font-weight);
 `,[W("& +",[t("content",{marginTop:"9px"})])]),t("content",{transition:"color .3s var(--n-bezier)",fontSize:"var(--n-font-size)"})]),t("icon",{transition:"color .3s var(--n-bezier)"})]);const ie={...k.props,title:String,showIcon:{type:Boolean,default:!0},type:{type:String,default:"default"},bordered:{type:Boolean,default:!0},closable:Boolean,onClose:Function,onAfterLeave:Function,onAfterHide:Function};var he=D({name:"Alert",inheritAttrs:!1,props:ie,slots:Object,setup(e){const{mergedClsPrefixRef:a,mergedBorderedRef:b,inlineThemeDisabled:u,mergedRtlRef:g}=N(e),m=k("Alert","-alert",ae,U,e,a),A=q("Alert",g,a),p=x(()=>{const{common:{cubicBezierEaseInOut:c},self:r}=m.value,{fontSize:$,borderRadius:B,titleFontWeight:P,lineHeight:w,iconSize:I,iconMargin:y,iconMarginRtl:T,closeIconSize:S,closeBorderRadius:L,closeSize:E,closeMargin:H,closeMarginRtl:M,padding:V}=r,{type:n}=e,{left:F,right:j}=se(y);return{"--n-bezier":c,"--n-color":r[s("color",n)],"--n-close-icon-size":S,"--n-close-border-radius":L,"--n-close-color-hover":r[s("closeColorHover",n)],"--n-close-color-pressed":r[s("closeColorPressed",n)],"--n-close-icon-color":r[s("closeIconColor",n)],"--n-close-icon-color-hover":r[s("closeIconColorHover",n)],"--n-close-icon-color-pressed":r[s("closeIconColorPressed",n)],"--n-icon-color":r[s("iconColor",n)],"--n-border":r[s("border",n)],"--n-title-text-color":r[s("titleTextColor",n)],"--n-content-text-color":r[s("contentTextColor",n)],"--n-line-height":w,"--n-border-radius":B,"--n-font-size":$,"--n-title-font-weight":P,"--n-icon-size":I,"--n-icon-margin":y,"--n-icon-margin-rtl":T,"--n-close-size":E,"--n-close-margin":H,"--n-close-margin-rtl":M,"--n-padding":V,"--n-icon-margin-left":F,"--n-icon-margin-right":j}}),C=u?J("alert",x(()=>e.type[0]),p,e):void 0,z=Q(!0),R=()=>{const{onAfterLeave:c,onAfterHide:r}=e;c&&c(),r&&r()};return{rtlEnabled:A,mergedClsPrefix:a,mergedBordered:b,visible:z,handleCloseClick:()=>{Promise.resolve(e.onClose?.()).then(c=>{c!==!1&&(z.value=!1)})},handleAfterLeave:()=>{R()},mergedTheme:m,cssVars:u?void 0:p,themeClass:C?.themeClass,onRender:C?.onRender}},render(){return this.onRender?.(),o(),l(K,{onAfterLeave:this.handleAfterLeave},{default:()=>{const{mergedClsPrefix:e,$slots:a}=this,b={class:[`${e}-alert`,this.themeClass,this.closable&&`${e}-alert--closable`,this.showIcon&&`${e}-alert--show-icon`,!this.title&&this.closable&&`${e}-alert--right-adjust`,this.rtlEnabled&&`${e}-alert--rtl`],style:this.cssVars,role:"alert"};return this.visible?(o(),h("div",_({key:1},_(this.$attrs,b)),[i(()=>this.closable&&(o(),l(Z,{clsPrefix:e,class:f(`${e}-alert__close`),onClick:this.handleCloseClick},null,8,["clsPrefix","class","onClick"]))),i(()=>this.bordered&&(o(),h("div",{class:f(`${e}-alert__border`)},null,2))),i(()=>this.showIcon&&(o(),h("div",{class:f(`${e}-alert__icon`),"aria-hidden":"true"},[i(()=>te(a.icon,()=>[(o(),l(X,{clsPrefix:e},{default:()=>{switch(this.type){case"success":return o(),l(oe,{key:3});case"info":return o(),l(re,{key:4});case"warning":return o(),l(ee,{key:5});case"error":return o(),l(Y,{key:6});default:return null}}},1032,["clsPrefix"]))]))],2))),G("div",{class:f([`${e}-alert-body`,this.mergedBordered&&`${e}-alert-body--bordered`])},[i(()=>ne(a.header,u=>{const g=u||this.title;return g?(o(),h("div",{key:2,class:f(`${e}-alert-body__title`)},[i(()=>g)],2)):null})),i(()=>a.default&&(o(),h("div",{class:f(`${e}-alert-body__content`)},[i(()=>a.default())],2)))],2)],16)):null}},1032,["onAfterLeave"])}});export{he as A};
