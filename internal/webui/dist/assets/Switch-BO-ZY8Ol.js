import{s as G,ak as i,aD as X,L as M,M as l,N as Y,d as ve,B as Q,g as u,c as m,a as p,O as r,y as n,e as q,x as J,A as ge,r as j,D as we,n as P,b2 as me,H as pe,aG as ye,b3 as ke,U as v,G as xe}from"./index-Bn7Z6giM.js";import{i as A,d as g,b as Se,c as E,p as O,e as s}from"./use-message-DPw4keui.js";import{a as _e}from"./Space-B4-DWySZ.js";var ze=G("switch",`
 height: var(--n-height);
 min-width: var(--n-width);
 vertical-align: middle;
 user-select: none;
 -webkit-user-select: none;
 display: inline-flex;
 outline: none;
 justify-content: center;
 align-items: center;
`,[i("children-placeholder",`
 height: var(--n-rail-height);
 display: flex;
 flex-direction: column;
 overflow: hidden;
 pointer-events: none;
 visibility: hidden;
 `),i("rail-placeholder",`
 display: flex;
 flex-wrap: none;
 `),i("button-placeholder",`
 width: calc(1.75 * var(--n-rail-height));
 height: var(--n-rail-height);
 `),G("base-loading",`
 position: absolute;
 top: 50%;
 left: 50%;
 transform: translateX(-50%) translateY(-50%);
 font-size: calc(var(--n-button-width) - 4px);
 color: var(--n-loading-color);
 transition: color .3s var(--n-bezier);
 `,[X({left:"50%",top:"50%",originalTransform:"translateX(-50%) translateY(-50%)"})]),i("checked, unchecked",`
 transition: color .3s var(--n-bezier);
 color: var(--n-text-color);
 box-sizing: border-box;
 position: absolute;
 white-space: nowrap;
 top: 0;
 bottom: 0;
 display: flex;
 align-items: center;
 line-height: 1;
 `),i("checked",`
 right: 0;
 padding-right: calc(1.25 * var(--n-rail-height) - var(--n-offset));
 `),i("unchecked",`
 left: 0;
 justify-content: flex-end;
 padding-left: calc(1.25 * var(--n-rail-height) - var(--n-offset));
 `),M("&:focus",[i("rail",`
 box-shadow: var(--n-box-shadow-focus);
 `)]),l("round",[i("rail","border-radius: calc(var(--n-rail-height) / 2);",[i("button","border-radius: calc(var(--n-button-height) / 2);")])]),Y("disabled",[Y("icon",[l("rubber-band",[l("pressed",[i("rail",[i("button","max-width: var(--n-button-width-pressed);")])]),i("rail",[M("&:active",[i("button","max-width: var(--n-button-width-pressed);")])]),l("active",[l("pressed",[i("rail",[i("button","left: calc(100% - var(--n-offset) - var(--n-button-width-pressed));")])]),i("rail",[M("&:active",[i("button","left: calc(100% - var(--n-offset) - var(--n-button-width-pressed));")])])])])])]),l("active",[i("rail",[i("button","left: calc(100% - var(--n-button-width) - var(--n-offset))")])]),i("rail",`
 overflow: hidden;
 height: var(--n-rail-height);
 min-width: var(--n-rail-width);
 border-radius: var(--n-rail-border-radius);
 cursor: pointer;
 position: relative;
 transition:
 opacity .3s var(--n-bezier),
 background .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier);
 background-color: var(--n-rail-color);
 `,[i("button-icon",`
 color: var(--n-icon-color);
 transition: color .3s var(--n-bezier);
 font-size: calc(var(--n-button-height) - 4px);
 position: absolute;
 left: 0;
 right: 0;
 top: 0;
 bottom: 0;
 display: flex;
 justify-content: center;
 align-items: center;
 line-height: 1;
 `,[X()]),i("button",`
 align-items: center; 
 top: var(--n-offset);
 left: var(--n-offset);
 height: var(--n-button-height);
 width: var(--n-button-width-pressed);
 max-width: var(--n-button-width);
 border-radius: var(--n-button-border-radius);
 background-color: var(--n-button-color);
 box-shadow: var(--n-button-box-shadow);
 box-sizing: border-box;
 cursor: inherit;
 content: "";
 position: absolute;
 transition:
 background-color .3s var(--n-bezier),
 left .3s var(--n-bezier),
 opacity .3s var(--n-bezier),
 max-width .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier);
 `)]),l("active",[i("rail","background-color: var(--n-rail-color-active);")]),l("loading",[i("rail",`
 cursor: wait;
 `)]),l("disabled",[i("rail",`
 cursor: not-allowed;
 opacity: .5;
 `)])]);const $e=["aria-checked","tabindex","onClick","onFocus","onBlur","onKeyup","onKeydown"],Be={...Q.props,size:String,value:{type:[String,Number,Boolean],default:void 0},loading:Boolean,defaultValue:{type:[String,Number,Boolean],default:!1},disabled:{type:Boolean,default:void 0},round:{type:Boolean,default:!0},"onUpdate:value":[Function,Array],onUpdateValue:[Function,Array],checkedValue:{type:[String,Number,Boolean],default:!0},uncheckedValue:{type:[String,Number,Boolean],default:!1},railStyle:Function,rubberBand:{type:Boolean,default:!0},spinProps:Object,onChange:[Function,Array]};let z;var Fe=ve({name:"Switch",props:Be,slots:Object,setup(e){z===void 0&&(typeof CSS<"u"?typeof CSS.supports<"u"?z=CSS.supports("width","max(1px)"):z=!1:z=!0);const{mergedClsPrefixRef:$,inlineThemeDisabled:y,mergedComponentPropsRef:T}=ge(e),D=Q("Switch","-switch",ze,ke,e,$),w=Se(e,{mergedSize(t){if(e.size!==void 0)return e.size;if(t)return t.mergedSize.value;const b=T?.value?.Switch?.size;return b||"medium"}}),{mergedSizeRef:k,mergedDisabledRef:h}=w,x=j(e.defaultValue),B=xe(e,"value"),f=_e(B,x),C=P(()=>f.value===e.checkedValue),a=j(!1),o=j(!1),S=P(()=>{const{railStyle:t}=e;if(t)return t({focused:o.value,checked:C.value})});function R(t){const{"onUpdate:value":b,onChange:V,onUpdateValue:F}=e,{nTriggerFormInput:K,nTriggerFormChange:W}=w;b&&E(b,t),F&&E(F,t),V&&E(V,t),x.value=t,K(),W()}function Z(){const{nTriggerFormFocus:t}=w;t()}function ee(){const{nTriggerFormBlur:t}=w;t()}function te(){e.loading||h.value||(f.value!==e.checkedValue?R(e.checkedValue):R(e.uncheckedValue))}function ie(){o.value=!0,Z()}function ae(){o.value=!1,ee(),a.value=!1}function ne(t){e.loading||h.value||t.key===" "&&(f.value!==e.checkedValue?R(e.checkedValue):R(e.uncheckedValue),a.value=!1)}function oe(t){e.loading||h.value||t.key===" "&&(t.preventDefault(),a.value=!0)}const I=P(()=>{const{value:t}=k,{self:{opacityDisabled:b,railColor:V,railColorActive:F,buttonBoxShadow:K,buttonColor:W,boxShadowFocus:re,loadingColor:le,textColor:se,iconColor:ce,[v("buttonHeight",t)]:c,[v("buttonWidth",t)]:de,[v("buttonWidthPressed",t)]:ue,[v("railHeight",t)]:d,[v("railWidth",t)]:_,[v("railBorderRadius",t)]:he,[v("buttonBorderRadius",t)]:fe},common:{cubicBezierEaseInOut:be}}=D.value;let N,U,H;return z?(N=`calc((${d} - ${c}) / 2)`,U=`max(${d}, ${c})`,H=`max(${_}, calc(${_} + ${c} - ${d}))`):(N=O((s(d)-s(c))/2),U=O(Math.max(s(d),s(c))),H=s(d)>s(c)?_:O(s(_)+s(c)-s(d))),{"--n-bezier":be,"--n-button-border-radius":fe,"--n-button-box-shadow":K,"--n-button-color":W,"--n-button-width":de,"--n-button-width-pressed":ue,"--n-button-height":c,"--n-height":U,"--n-offset":N,"--n-opacity-disabled":b,"--n-rail-border-radius":he,"--n-rail-color":V,"--n-rail-color-active":F,"--n-rail-height":d,"--n-rail-width":_,"--n-width":H,"--n-box-shadow-focus":re,"--n-loading-color":le,"--n-text-color":se,"--n-icon-color":ce}}),L=y?we("switch",P(()=>k.value[0]),I,e):void 0;return{handleClick:te,handleBlur:ae,handleFocus:ie,handleKeyup:ne,handleKeydown:oe,mergedRailStyle:S,pressed:a,mergedClsPrefix:$,mergedValue:f,checked:C,mergedDisabled:h,cssVars:y?void 0:I,themeClass:L?.themeClass,onRender:L?.onRender}},render(){const{mergedClsPrefix:e,mergedDisabled:$,checked:y,mergedRailStyle:T,onRender:D,$slots:w}=this;D?.();const{checked:k,unchecked:h,icon:x,"checked-icon":B,"unchecked-icon":f}=w,C=!(A(x)&&A(B)&&A(f));return u(),m("div",{role:"switch","aria-checked":y,class:n([`${e}-switch`,this.themeClass,C&&`${e}-switch--icon`,y&&`${e}-switch--active`,$&&`${e}-switch--disabled`,this.round&&`${e}-switch--round`,this.loading&&`${e}-switch--loading`,this.pressed&&`${e}-switch--pressed`,this.rubberBand&&`${e}-switch--rubber-band`]),tabindex:this.mergedDisabled?void 0:0,style:J(this.cssVars),onClick:this.handleClick,onFocus:this.handleFocus,onBlur:this.handleBlur,onKeyup:this.handleKeyup,onKeydown:this.handleKeydown},[p("div",{class:n(`${e}-switch__rail`),"aria-hidden":"true",style:J(T)},[r(()=>g(k,a=>g(h,o=>a||o?(u(),m("div",{key:4,"aria-hidden":!0,class:n(`${e}-switch__children-placeholder`)},[p("div",{class:n(`${e}-switch__rail-placeholder`)},[p("div",{class:n(`${e}-switch__button-placeholder`)},null,2),r(()=>a)],2),p("div",{class:n(`${e}-switch__rail-placeholder`)},[p("div",{class:n(`${e}-switch__button-placeholder`)},null,2),r(()=>o)],2)],2)):null))),p("div",{class:n(`${e}-switch__button`)},[r(()=>g(x,a=>g(B,o=>g(f,S=>(u(),q(ye,null,{default:()=>this.loading?(u(),q(me,pe({key:"loading",clsPrefix:e,strokeWidth:20},this.spinProps),null,16,["clsPrefix"])):this.checked&&(o||a)?(u(),m("div",{class:n(`${e}-switch__button-icon`),key:o?"checked-icon":"icon"},[r(()=>o||a)],2)):!this.checked&&(S||a)?(u(),m("div",{class:n(`${e}-switch__button-icon`),key:S?"unchecked-icon":"icon"},[r(()=>S||a)],2)):null},1024)))))),r(()=>g(k,a=>a&&(u(),m("div",{key:"checked",class:n(`${e}-switch__checked`)},[r(()=>a)],2)))),r(()=>g(h,a=>a&&(u(),m("div",{key:"unchecked",class:n(`${e}-switch__unchecked`)},[r(()=>a)],2))))],2)],6)],46,$e)}});export{Fe as S};
